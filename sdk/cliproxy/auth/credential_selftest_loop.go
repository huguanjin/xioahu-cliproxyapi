package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// credentialSelfTestLoop walks the credential pool on a timer and probes what it
// finds. It owns two pieces of runtime state that are deliberately not persisted:
// when each credential was last probed, and the outcome of the most recent run.
// A restart simply re-probes everything, which is the safe direction.
type credentialSelfTestLoop struct {
	manager *Manager
	options SelfTestOptions

	// scheduleEnabled mirrors options.Enabled but can be toggled while running.
	// The loop keeps ticking when the schedule is off so a manual trigger has a
	// live loop to run on.
	scheduleEnabled bool

	mu         sync.Mutex
	lastProbed map[string]time.Time
	lastReport *SelfTestReport
	// running guards against overlapping manual runs; a second trigger while one
	// run is in flight is refused rather than queued.
	running bool
}

// SelfTestReport summarises one completed self-test run.
type SelfTestReport struct {
	// StartedAt and FinishedAt bound the run.
	StartedAt  time.Time
	FinishedAt time.Time
	// Manual records whether an operator triggered the run rather than the schedule.
	Manual bool
	// Concurrency and Timeout are the settings the run used, so an operator can
	// tell a slow run apart from a throttled one.
	Concurrency int
	Timeout     time.Duration
	// Probed counts credentials the run actually probed.
	Probed int
	// Skipped counts credentials the run passed over: already cooling, disabled,
	// or off schedule. It is the pool size minus what was probed, plus any
	// credential whose provider has no probe.
	Skipped int
	// Verdict tallies probed credentials by what the run concluded.
	Verdict SelfTestVerdictCounts
	// Failures lists the credentials this run flagged, most recent first.
	Failures []SelfTestFailure
}

// SelfTestVerdictCounts tallies a run's conclusions.
type SelfTestVerdictCounts struct {
	// Healthy credentials answered 2xx.
	Healthy int
	// Cooling credentials are now parked in cooldown by this run.
	Cooling int
	// Deterministic credentials failed in a way that is the credential's own
	// fault and consumed one of their escalation strikes.
	Deterministic int
	// Escalated credentials crossed the deterministic threshold and had their
	// cooldown pushed out.
	Escalated int
	// Transient covers probe failures our side could not attribute to the
	// credential (transport errors, 5xx, our own deadline).
	Transient int
}

// SelfTestFailure describes one credential a run flagged.
type SelfTestFailure struct {
	AuthID string
	// Provider and Label are copied so a report is readable without a lookup.
	Provider string
	Label    string
	// StatusCode is the upstream status; 0 means the probe never reached it.
	StatusCode int
	// Message is the upstream body or transport error, already trimmed.
	Message string
	// Kind is how the failure was classified.
	Kind SelfTestFailureKind
	// Strikes is the credential's consecutive deterministic failure count.
	Strikes int
	// CooldownUntil is the deadline this run left on the credential.
	CooldownUntil time.Time
}

// SelfTestFailureKind classifies why a probe failed.
type SelfTestFailureKind string

const (
	// SelfTestFailureTransient covers failures that say nothing about whether the
	// credential is valid: transport errors, our own deadline, 5xx.
	SelfTestFailureTransient SelfTestFailureKind = "transient"
	// SelfTestFailureDeterministic covers failures the credential itself caused.
	SelfTestFailureDeterministic SelfTestFailureKind = "deterministic"
)

// StartCredentialSelfTest launches the background loop that probes provider
// credentials, so a dead credential drains out of rotation before live traffic
// discovers it. The loop runs even when options.Enabled is false: the schedule
// stays off, but an operator can still trigger a run through the management API.
//
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartCredentialSelfTest(parent context.Context, options SelfTestOptions) {
	if m == nil {
		return
	}
	options = options.Normalize()
	// Publish before the loop starts so ApplyCredentialSelfTest can read the
	// escalation thresholds without reaching for the loop's own state.
	m.selfTestOptionValue.Store(options)

	m.mu.Lock()
	cancelPrev := m.selfTestCancel
	m.selfTestCancel = nil
	m.selfTestLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)

	loop := &credentialSelfTestLoop{
		manager:         m,
		options:         options,
		scheduleEnabled: options.Enabled,
		lastProbed:      make(map[string]time.Time),
	}
	m.mu.Lock()
	m.selfTestCancel = cancelCtx
	m.selfTestLoop = loop
	m.mu.Unlock()

	go loop.run(ctx)
}

// StopCredentialSelfTest cancels the background self-test loop, if running.
func (m *Manager) StopCredentialSelfTest() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.selfTestCancel
	m.selfTestCancel = nil
	m.selfTestLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// SetCredentialSelfTestScheduleEnabled turns the periodic schedule on or off
// without tearing the loop down, so a manual trigger keeps working while the
// schedule is off.
func (m *Manager) SetCredentialSelfTestScheduleEnabled(enabled bool) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	if loop == nil {
		return false
	}
	loop.mu.Lock()
	loop.scheduleEnabled = enabled
	loop.mu.Unlock()
	return true
}

// CredentialSelfTestScheduleEnabled reports whether the periodic schedule is on.
func (m *Manager) CredentialSelfTestScheduleEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	if loop == nil {
		return false
	}
	loop.mu.Lock()
	defer loop.mu.Unlock()
	return loop.scheduleEnabled
}

// CredentialSelfTestRunning reports whether a self-test loop exists. The
// schedule may be off while the loop is live, which is exactly the state a manual
// trigger depends on.
func (m *Manager) CredentialSelfTestRunning() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	return loop != nil
}

// CredentialSelfTestOptions returns the options the loop carries. A caller that
// has to start a loop itself, such as the management API after a stop, reads them
// here so the YAML-derived concurrency and period survive instead of silently
// reverting to defaults.
func (m *Manager) CredentialSelfTestOptions() SelfTestOptions {
	return m.selfTestOptions()
}

// LastCredentialSelfTestReport returns the most recent completed run, or nil if
// no run has finished since start.
func (m *Manager) LastCredentialSelfTestReport() *SelfTestReport {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	if loop == nil {
		return nil
	}
	loop.mu.Lock()
	defer loop.mu.Unlock()
	if loop.lastReport == nil {
		return nil
	}
	// Hand out a copy: the report is read by an HTTP handler while the loop may
	// be starting the next run.
	report := *loop.lastReport
	report.Failures = append([]SelfTestFailure(nil), loop.lastReport.Failures...)
	return &report
}

// RunCredentialSelfTestNow probes immediately, ignoring both the schedule and the
// per-credential period. It returns the run's report, or nil if a run was already
// in flight or the loop is not running.
//
// The per-credential period is deliberately bypassed: an operator pressing the
// button is asking "what is true right now", and honouring the period would hand
// back a report that mostly re-states the last scheduled run.
func (m *Manager) RunCredentialSelfTestNow(parent context.Context) *SelfTestReport {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	if loop == nil {
		return nil
	}
	return loop.runNow(parent)
}

func (l *credentialSelfTestLoop) run(ctx context.Context) {
	ticker := time.NewTicker(l.options.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			enabled := l.scheduleEnabled
			l.mu.Unlock()
			if !enabled {
				continue
			}
			l.runOnce(ctx, false, true)
		}
	}
}

func (l *credentialSelfTestLoop) runNow(parent context.Context) *SelfTestReport {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return nil
	}
	l.running = true
	l.mu.Unlock()
	// A manual run must not inherit the previous run's cancellation, so it gets
	// its own bounded context derived from the caller's.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	return l.runOnce(ctx, true, false)
}

// runOnce performs one probe sweep and stores its report. It always clears the
// running flag, including on a panic, so one bad probe cannot wedge the loop.
func (l *credentialSelfTestLoop) runOnce(ctx context.Context, manual bool, markProbed bool) *SelfTestReport {
	l.mu.Lock()
	if l.running && !manual {
		// A manual run is in flight; skip this tick rather than pile on.
		l.mu.Unlock()
		return nil
	}
	if manual {
		l.running = true
	}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
	}()

	startedAt := time.Now()
	candidates, skipped := l.candidates(time.Now(), markProbed)
	report := &SelfTestReport{
		StartedAt:   startedAt,
		Manual:      manual,
		Concurrency: l.options.Concurrency,
		Timeout:     l.options.Timeout,
		Skipped:     skipped,
	}

	results := make(chan SelfTestFailure, len(candidates))
	limit := l.options.Concurrency
	if limit <= 0 {
		limit = 1
	}
	semaphore := make(chan struct{}, limit)
	var waitGroup sync.WaitGroup
	var launched atomic.Int64
	for _, auth := range candidates {
		if ctx.Err() != nil {
			break
		}
		waitGroup.Add(1)
		semaphore <- struct{}{}
		launched.Add(1)
		go func(auth *Auth) {
			defer waitGroup.Done()
			defer func() { <-semaphore }()
			if failure, counted := l.probeOne(ctx, auth); counted {
				results <- failure
			}
		}(auth)
	}
	waitGroup.Wait()
	close(results)

	// Every launched probe either reports a failure or came back clean, so the
	// launched count is the denominator and the failures are subtracted from it.
	report.Probed = int(launched.Load())
	for failure := range results {
		switch failure.Kind {
		case SelfTestFailureDeterministic:
			report.Verdict.Deterministic++
			if failure.Strikes >= l.options.DeterministicFailureThreshold {
				report.Verdict.Escalated++
			}
		default:
			report.Verdict.Transient++
		}
		if failure.Kind == SelfTestFailureDeterministic {
			report.Verdict.Cooling++
		}
		report.Failures = append(report.Failures, failure)
	}
	report.Verdict.Healthy = report.Probed - report.Verdict.Deterministic - report.Verdict.Transient
	if report.Verdict.Healthy < 0 {
		report.Verdict.Healthy = 0
	}
	report.FinishedAt = time.Now()

	l.mu.Lock()
	l.lastReport = report
	l.mu.Unlock()

	log.Infof("credential self-test: run complete manual=%v probed=%d skipped=%d healthy=%d cooling=%d deterministic=%d escalated=%d transient=%d in %s",
		manual, report.Probed, report.Skipped, report.Verdict.Healthy, report.Verdict.Cooling,
		report.Verdict.Deterministic, report.Verdict.Escalated, report.Verdict.Transient,
		report.FinishedAt.Sub(report.StartedAt).Round(time.Millisecond))
	return report
}

// candidates returns the credentials to probe and the number passed over.
// markProbed records the attempt even for a probe that never reports, so a probe
// that hangs still respects the period; a manual run skips it so a scheduled run
// is not starved by an operator pressing the button repeatedly.
func (l *credentialSelfTestLoop) candidates(now time.Time, markProbed bool) ([]*Auth, int) {
	l.manager.mu.RLock()
	list := make([]*Auth, 0, len(l.manager.auths))
	for _, auth := range l.manager.auths {
		list = append(list, auth.Clone())
	}
	l.manager.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	due := make([]*Auth, 0, len(list))
	skipped := 0
	for _, auth := range list {
		if !selfTestEligible(auth) {
			skipped++
			continue
		}
		if markProbed {
			authID := strings.TrimSpace(auth.ID)
			if last, ok := l.lastProbed[authID]; ok && now.Sub(last) < l.options.PerCredentialPeriod {
				skipped++
				continue
			}
			l.lastProbed[authID] = now
		}
		due = append(due, auth)
	}
	return due, skipped
}

// selfTestEligible reports whether a credential is worth probing. Disabled
// credentials are out of rotation by operator intent, and a credential already
// serving a cooldown needs no second opinion: the scheduler is already skipping
// it, and the probe would only rediscover the same rejection.
func selfTestEligible(auth *Auth) bool {
	if auth == nil || auth.Disabled {
		return false
	}
	if strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if auth.Unavailable || auth.Quota.Exceeded {
		return false
	}
	now := time.Now()
	if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		return false
	}
	return true
}

// probeOne probes a single credential. The bool reports whether the outcome is a
// failure worth putting in the run's report; a healthy credential and one whose
// provider has no probe both return false.
func (l *credentialSelfTestLoop) probeOne(parent context.Context, auth *Auth) (SelfTestFailure, bool) {
	provider := strings.TrimSpace(auth.Provider)
	tester, ok := l.manager.credentialSelfTester(provider)
	if !ok {
		// Not every provider has a credential-check endpoint that is safe to call
		// on a timer; skip it rather than guessing at a probe.
		return SelfTestFailure{}, false
	}
	ctx, cancel := context.WithTimeout(parent, l.options.Timeout)
	defer cancel()

	result, errProbe := tester.SelfTestCredential(ctx, auth)
	if errProbe != nil {
		// A probe that failed to run is usually our problem, not the
		// credential's. An executor that attaches an HTTP status is the
		// exception: it is reporting a rejection it already got from upstream
		// (a missing or refused refresh token, for example), which is exactly
		// the deterministic failure the escalation threshold exists for. Routing
		// it through ApplyCredentialSelfTest lets the shared classifier decide.
		if status, ok := selfTestErrorStatus(errProbe); ok {
			return l.manager.ApplyCredentialSelfTest(parent, CredentialSelfTestResult{
				AuthID:     auth.ID,
				Provider:   provider,
				StatusCode: status,
				Message:    errProbe.Error(),
				ProbeError: true,
			}), true
		}
		log.Debugf("credential self-test: probe %s %s failed to run: %v", provider, auth.ID, errProbe)
		return l.recordTransient(auth, errProbe.Error()), true
	}
	if result == nil {
		return SelfTestFailure{}, false
	}
	if strings.TrimSpace(result.AuthID) == "" {
		result.AuthID = auth.ID
	}
	if strings.TrimSpace(result.Provider) == "" {
		result.Provider = provider
	}
	return l.manager.ApplyCredentialSelfTest(parent, *result), true
}

func (m *Manager) credentialSelfTester(provider string) (CredentialSelfTester, bool) {
	executor, ok := m.Executor(provider)
	if !ok || executor == nil {
		return nil, false
	}
	tester, ok := executor.(CredentialSelfTester)
	if !ok || tester == nil {
		return nil, false
	}
	return tester, true
}

// selfTestErrorStatus extracts an HTTP status from a probe error, if it carries
// one. Executors report upstream rejections they hit while acquiring a token as
// errors rather than results, and those still carry a status; a structural check
// keeps this package free of any executor import.
//
// Only 4xx counts. A 5xx reported through an error is upstream trouble, not the
// credential's verdict, and must not cool the credential down — the classifier
// would say the same, so filtering here keeps the two paths consistent.
func selfTestErrorStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var withStatus interface{ StatusCode() int }
	if !errors.As(err, &withStatus) {
		return 0, false
	}
	status := withStatus.StatusCode()
	if status < http.StatusBadRequest || status >= http.StatusInternalServerError {
		return 0, false
	}
	return status, true
}

// selfTestOptions returns the options the running loop uses, falling back to
// defaults when none were published. It exists for ApplyCredentialSelfTest,
// which reads the escalation thresholds while holding mu; the options live in an
// atomic.Value precisely so this read cannot deadlock against that lock.
func (m *Manager) selfTestOptions() SelfTestOptions {
	if m == nil {
		return DefaultSelfTestOptions()
	}
	value := m.selfTestOptionValue.Load()
	if value == nil {
		return DefaultSelfTestOptions()
	}
	options, ok := value.(SelfTestOptions)
	if !ok {
		return DefaultSelfTestOptions()
	}
	return options
}

// recordTransient builds the report entry for a probe that never produced an
// upstream verdict. It touches no scheduling state: our own deadline expiring or
// a dial error says nothing about the credential.
func (l *credentialSelfTestLoop) recordTransient(auth *Auth, message string) SelfTestFailure {
	if auth == nil {
		return SelfTestFailure{Kind: SelfTestFailureTransient, Message: message}
	}
	return SelfTestFailure{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Label:    auth.Label,
		Kind:     SelfTestFailureTransient,
		Message:  message,
	}
}
