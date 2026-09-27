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

	// ctx is the loop's lifetime, used to give background runs something to
	// cancel against without tying them to the request that triggered them.
	ctx context.Context

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
	// progress is the live view of the run in flight, nil when idle. A pool the
	// size of a production deployment takes hours to sweep, so the run can no
	// longer be reported only once it finishes.
	progress *SelfTestProgress
}

// SelfTestProgress is the live state of a run that is still in flight. The counts
// grow as probes land, so a caller polling the status endpoint can show movement
// instead of an indefinite spinner.
type SelfTestProgress struct {
	// StartedAt is when the run began.
	StartedAt time.Time
	// Manual records whether an operator triggered the run rather than the schedule.
	Manual bool
	// Concurrency is the setting the run is using.
	Concurrency int
	// Total is how many credentials this run will probe.
	Total int
	// Completed counts probes that have landed, healthy or not.
	Completed int
	// Verdict tallies completed probes. Healthy plus the failure kinds equals
	// Completed, so a progress bar can be drawn from any pair.
	Verdict SelfTestVerdictCounts
	// Failures lists what the run has flagged so far, capped at
	// SelfTestProgressFailureLimit so a run full of dead credentials cannot grow
	// the status response without bound.
	Failures []SelfTestFailure
}

// SelfTestProgressFailureLimit caps the failures carried by a live progress
// snapshot. The finished report keeps every failure; only the in-flight view is
// bounded, because it is serialised on every poll.
const SelfTestProgressFailureLimit = 200

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
		ctx:             ctx,
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

// CredentialSelfTestProgress returns a snapshot of the run in flight, or nil when
// no run is in progress. The run is long enough that the UI needs to show it
// moving, so this is read on every status poll.
func (m *Manager) CredentialSelfTestProgress() *SelfTestProgress {
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
	if loop.progress == nil {
		return nil
	}
	// Copy: the loop mutates the original as probes land while the handler reads.
	progress := *loop.progress
	progress.Failures = append([]SelfTestFailure(nil), loop.progress.Failures...)
	return &progress
}

// RunCredentialSelfTestNow starts a sweep in the background, ignoring both the
// schedule and the per-credential period. It reports whether the run was
// accepted; a run already in flight is refused rather than queued, and a missing
// loop is refused too.
//
// The per-credential period is deliberately bypassed: an operator pressing the
// button is asking "what is true right now", and honouring the period would hand
// back a report that mostly re-states the last scheduled run.
//
// The run does not borrow the caller's context. A sweep over a production pool
// takes far longer than an HTTP request may live, so inheriting it would have the
// browser closing the tab kill the run. The loop's own lifetime governs instead.
func (m *Manager) RunCredentialSelfTestNow() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	loop := m.selfTestLoop
	m.mu.Unlock()
	if loop == nil {
		return false
	}
	return loop.runNow()
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

// runNow starts a manual sweep in the background. It reports whether the run was
// accepted: a second trigger while one is in flight is refused rather than
// queued. The sweep outlives the HTTP request that asked for it, so callers poll
// CredentialSelfTestProgress rather than waiting on a return value.
func (l *credentialSelfTestLoop) runNow() bool {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return false
	}
	l.running = true
	ctx := l.ctx
	l.mu.Unlock()

	go func() {
		// No parent context: see RunCredentialSelfTestNow. The loop's own cancel
		// is the only thing that may stop a sweep already under way.
		l.runOnce(ctx, true, false)
	}()
	return true
}

// runOnce performs one probe sweep and stores its report. It always clears the
// running flag, including on a panic, so one bad probe cannot wedge the loop.
func (l *credentialSelfTestLoop) runOnce(ctx context.Context, manual bool, markProbed bool) *SelfTestReport {
	l.mu.Lock()
	if l.running {
		if manual {
			// runNow already claimed the flag for this run.
			l.mu.Unlock()
		} else {
			// A manual run is in flight; skip this tick rather than pile on.
			l.mu.Unlock()
			return nil
		}
	} else {
		l.running = true
		l.mu.Unlock()
	}

	defer func() {
		l.mu.Lock()
		l.running = false
		l.progress = nil
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

	l.mu.Lock()
	l.progress = &SelfTestProgress{
		StartedAt:   startedAt,
		Manual:      manual,
		Concurrency: l.options.Concurrency,
		Total:       len(candidates),
	}
	l.mu.Unlock()

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
			failure, counted := l.probeOne(ctx, auth)
			if counted {
				results <- failure
				l.noteProgress(auth, failure)
				return
			}
			// A probe that reports nothing is still a completed probe, so the
			// progress view has to count it or the bar stalls below 100%.
			l.noteProgress(auth, SelfTestFailure{})
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
	l.finishProgress(report)

	l.mu.Lock()
	l.lastReport = report
	l.mu.Unlock()

	log.Infof("credential self-test: run complete manual=%v probed=%d skipped=%d healthy=%d cooling=%d deterministic=%d escalated=%d transient=%d in %s",
		manual, report.Probed, report.Skipped, report.Verdict.Healthy, report.Verdict.Cooling,
		report.Verdict.Deterministic, report.Verdict.Escalated, report.Verdict.Transient,
		report.FinishedAt.Sub(report.StartedAt).Round(time.Millisecond))
	return report
}

// finishProgress marks the live view complete before it is cleared, so a status
// poll landing in the same instant as the final probe still sees the run at 100%
// rather than at whatever count the previous poll caught.
func (l *credentialSelfTestLoop) finishProgress(report *SelfTestReport) {
	if report == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.progress == nil {
		return
	}
	l.progress.Completed = report.Probed
	l.progress.Total = report.Probed
	l.progress.Verdict = report.Verdict
}

// noteProgress folds one landed probe into the live progress view. A zero
// failure means the probe came back clean and only counts toward the completed
// total; anything else is tallied the same way the finished report tallies it, so
// the running numbers and the final report agree.
func (l *credentialSelfTestLoop) noteProgress(auth *Auth, failure SelfTestFailure) {
	l.mu.Lock()
	defer l.mu.Unlock()
	progress := l.progress
	if progress == nil {
		// The run was torn down or replaced while this probe was in flight.
		return
	}
	progress.Completed++
	switch failure.Kind {
	case SelfTestFailureDeterministic:
		progress.Verdict.Deterministic++
		progress.Verdict.Cooling++
		if failure.Strikes >= l.options.DeterministicFailureThreshold {
			progress.Verdict.Escalated++
		}
	case SelfTestFailureTransient:
		progress.Verdict.Transient++
	default:
		progress.Verdict.Healthy++
	}
	if failure.Kind == "" {
		return
	}
	if len(progress.Failures) >= SelfTestProgressFailureLimit {
		return
	}
	if failure.Label == "" && auth != nil {
		failure.Label = auth.Label
	}
	progress.Failures = append(progress.Failures, failure)
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
