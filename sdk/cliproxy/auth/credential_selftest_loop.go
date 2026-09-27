package auth

import (
	"context"
	"errors"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// credentialSelfTestLoop walks the credential pool on a timer and probes what it
// finds. Only the most recent run is held in memory; how far each credential has
// backed off lives on the credential itself, so a restart resumes every cadence
// instead of re-probing the whole pool at once.
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
//
// Every counter is incremented where the verdict is decided, including Healthy.
// Deriving Healthy by subtracting the other counters from the probe total meant
// any credential whose outcome the tally did not recognise was silently reported
// as healthy — the most dangerous direction for the number an operator reads to
// decide whether the pool is fine.
type SelfTestVerdictCounts struct {
	// Healthy credentials answered 2xx to the probe that was actually run.
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
	// Validation counts credentials whose account still needs Google
	// verification. They are neither healthy nor dead, and the run leaves their
	// scheduling state untouched.
	Validation int
	// QuotaExhausted counts credentials the deep probe found with a spent
	// generation quota. They are also counted in Cooling, so this is a subset
	// rather than a sibling: it is the share of cooldowns that a cheap probe
	// would have missed.
	QuotaExhausted int
	// NotProbed counts credentials the run selected but could not probe at all,
	// because their provider has no credential-check endpoint. They were counted
	// as probed before, which made the total overstate what the run learned.
	NotProbed int
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
	// ForbiddenType is the 403 subtype the executor read from the body, empty
	// when the executor made no distinction.
	ForbiddenType string
	// ValidationURL is the verification link that came with a validation 403.
	ValidationURL string
	// Tier is which probe produced this failure. The run reports the share of
	// rejections that only the deep probe could see, so the tier has to survive
	// into the report.
	Tier CredentialSelfTestTier
}

// quotaExhausted reports whether this failure is a spent generation quota: the
// deep probe's own question, and one no cheap probe can answer.
func (f SelfTestFailure) quotaExhausted() bool {
	return f.Tier == SelfTestTierGeneration && f.StatusCode == http.StatusTooManyRequests
}

// SelfTestFailureKind classifies why a probe failed.
type SelfTestFailureKind string

const (
	// SelfTestFailureTransient covers failures that say nothing about whether the
	// credential is valid: transport errors, our own deadline, 5xx.
	SelfTestFailureTransient SelfTestFailureKind = "transient"
	// SelfTestFailureDeterministic covers failures the credential itself caused.
	SelfTestFailureDeterministic SelfTestFailureKind = "deterministic"
	// SelfTestFailureValidation covers the 403 the upstream sends when the
	// account owner still has to complete Google's verification step. It is
	// reported but drives no cooldown and no strike: the credential is not dead,
	// a human simply has to act.
	SelfTestFailureValidation SelfTestFailureKind = "validation"
)

// StartCredentialSelfTest launches the background loop that probes provider
// credentials, so a dead credential drains out of rotation before live traffic
// discovers it. The loop runs even when options.Enabled is false: the schedule
// stays off, but an operator can still trigger a run through the management API.
//
// The loop's lifetime is the manager's, not the caller's. It deliberately takes
// no context: the only in-tree caller is an HTTP handler, whose request context
// is cancelled the instant the handler returns its 202, and a loop bound to that
// context would be dead before its first probe. Shutdown goes through
// StopCredentialSelfTest instead.
//
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartCredentialSelfTest(options SelfTestOptions) {
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

	ctx, cancelCtx := context.WithCancel(context.Background())

	loop := &credentialSelfTestLoop{
		manager:         m,
		options:         options,
		ctx:             ctx,
		scheduleEnabled: options.Enabled,
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
	// Healthy and NotProbed are counted as probes land rather than derived at the
	// end, so an outcome the tally does not recognise can never be reported as a
	// healthy credential.
	var healthy atomic.Int64
	var notProbed atomic.Int64
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
			failure, outcome := l.probeOne(ctx, auth)
			switch outcome {
			case selfTestOutcomeFailed:
				results <- failure
			case selfTestOutcomeNotProbed:
				notProbed.Add(1)
			default:
				healthy.Add(1)
			}
			l.noteProgress(auth, failure, outcome)
		}(auth)
	}
	waitGroup.Wait()
	close(results)

	// Every launched probe is accounted for below: each one either failed, came
	// back clean, or could not be run. The three counters are incremented where
	// the outcome is decided, so their sum is the launched count and no
	// unclassified outcome can hide inside Healthy.
	report.Probed = int(launched.Load())
	for failure := range results {
		switch failure.Kind {
		case SelfTestFailureDeterministic:
			report.Verdict.Deterministic++
			report.Verdict.Cooling++
			if failure.quotaExhausted() {
				report.Verdict.QuotaExhausted++
			}
			if failure.Strikes >= l.options.DeterministicFailureThreshold {
				report.Verdict.Escalated++
			}
		case SelfTestFailureValidation:
			// Neither cooling nor deterministic: the run took no scheduling
			// action, so counting it as either would misreport the pool.
			report.Verdict.Validation++
		default:
			report.Verdict.Transient++
		}
		report.Failures = append(report.Failures, failure)
	}
	report.Verdict.Healthy = int(healthy.Load())
	report.Verdict.NotProbed = int(notProbed.Load())
	report.FinishedAt = time.Now()
	l.finishProgress(report)

	l.mu.Lock()
	l.lastReport = report
	l.mu.Unlock()

	log.Infof("credential self-test: run complete manual=%v probed=%d skipped=%d healthy=%d cooling=%d deterministic=%d escalated=%d validation=%d transient=%d quota_exhausted=%d not_probed=%d in %s",
		manual, report.Probed, report.Skipped, report.Verdict.Healthy, report.Verdict.Cooling,
		report.Verdict.Deterministic, report.Verdict.Escalated, report.Verdict.Validation, report.Verdict.Transient,
		report.Verdict.QuotaExhausted, report.Verdict.NotProbed,
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

// noteProgress folds one landed probe into the live progress view, tallied the
// same way the finished report tallies it so the running numbers and the final
// report agree.
func (l *credentialSelfTestLoop) noteProgress(auth *Auth, failure SelfTestFailure, outcome selfTestOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	progress := l.progress
	if progress == nil {
		// The run was torn down or replaced while this probe was in flight.
		return
	}
	progress.Completed++
	switch {
	case outcome == selfTestOutcomeNotProbed:
		progress.Verdict.NotProbed++
	case failure.Kind == SelfTestFailureDeterministic:
		progress.Verdict.Deterministic++
		progress.Verdict.Cooling++
		if failure.quotaExhausted() {
			progress.Verdict.QuotaExhausted++
		}
		if failure.Strikes >= l.options.DeterministicFailureThreshold {
			progress.Verdict.Escalated++
		}
	case failure.Kind == SelfTestFailureValidation:
		progress.Verdict.Validation++
	case failure.Kind == SelfTestFailureTransient:
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
//
// Due-ness comes from each credential's persisted next_probe_at rather than from
// an in-memory map, so a restart resumes the cadence every credential had earned
// instead of re-probing the whole pool at once. A credential whose next_probe_at
// is unset has never been probed and is due immediately, which is what lets a
// freshly imported pool be checked in one pass.
//
// markProbed stamps next_probe_at here, before the probe runs, so a probe that
// hangs still costs the credential its turn; the probe's own outcome overwrites
// the stamp with the cadence it earned. A manual run skips the stamp so an
// operator pressing the button does not starve the schedule of its next pass.
func (l *credentialSelfTestLoop) candidates(now time.Time, markProbed bool) ([]*Auth, int) {
	l.manager.mu.RLock()
	list := make([]*Auth, 0, len(l.manager.auths))
	for _, auth := range l.manager.auths {
		list = append(list, auth.Clone())
	}
	l.manager.mu.RUnlock()

	due := make([]*Auth, 0, len(list))
	skipped := 0
	for _, auth := range list {
		if !selfTestDue(auth, now, markProbed) {
			skipped++
			continue
		}
		due = append(due, auth)
	}
	if markProbed {
		l.stampProbed(due, now)
	}
	return due, skipped
}

// stampProbed advances next_probe_at for the credentials this sweep is about to
// probe, so a probe that hangs still costs its credential a turn.
//
// The stamp only ever moves a credential's next probe later, and only if the
// credential is still due: a probe from the previous sweep may have written the
// cadence it earned in the meantime, and that answer must not be overwritten with
// a placeholder. That is why the check is re-run under the lock rather than
// trusting the cloned snapshot.
func (l *credentialSelfTestLoop) stampProbed(candidates []*Auth, now time.Time) {
	if len(candidates) == 0 || l.manager == nil {
		return
	}
	l.manager.mu.Lock()
	for _, candidate := range candidates {
		auth, ok := l.manager.auths[candidate.ID]
		if !ok || auth == nil {
			continue
		}
		state := auth.SelfTestState()
		state.NextProbeAt = now.Add(l.options.PerCredentialPeriod)
		auth.SetSelfTestState(state)
		_ = l.manager.persist(context.Background(), auth)
	}
	l.manager.mu.Unlock()
}

// selfTestDue reports whether a credential should be probed in this sweep.
//
// The rule that matters here is that a credential is skipped only when it is out
// of the loop's reach, not when it is currently failing. Previously an
// unavailable or cooling credential was skipped, which meant a credential the
// loop itself had cooled down was never examined again: the failures that
// triggered the cooldown also removed the credential from the set that could ever
// clear it. A credential parked by the schedule keeps its place in the queue, on
// the slower cadence its verdict earned.
//
// The remaining skip reason is an operator disabling a credential: the loop does
// not second-guess that, and probing it would only burn a request to confirm an
// intentional state. An auto-disabled credential is the exception, because the
// loop is the one that parked it and the loop is the only thing that can lift it.
//
// respectCadence is false for a manual run: an operator pressing the button is
// asking what is true right now, and honouring a stored cadence would hand back a
// report that mostly restates the last scheduled sweep.
func selfTestDue(auth *Auth, now time.Time, respectCadence bool) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	state := auth.SelfTestState()
	if auth.Disabled && !state.AutoDisabled {
		return false
	}
	if !respectCadence || state.NextProbeAt.IsZero() {
		return true
	}
	return !state.NextProbeAt.After(now)
}

// selfTestOutcome is what a single probe produced, kept distinct from the failure
// it may carry. The run's tallies need to tell three cases apart that a single
// "did it fail" bool conflates: a credential that answered cleanly, one that was
// rejected, and one the run could not probe at all.
type selfTestOutcome int

const (
	// selfTestOutcomeProbed means the upstream answered and the credential passed.
	selfTestOutcomeProbed selfTestOutcome = iota
	// selfTestOutcomeFailed means the run recorded a failure against the credential.
	selfTestOutcomeFailed
	// selfTestOutcomeNotProbed means the provider has no probe to run, so the run
	// learned nothing about this credential.
	selfTestOutcomeNotProbed
)

// probeOne probes a single credential and reports what the run learned.
func (l *credentialSelfTestLoop) probeOne(parent context.Context, auth *Auth) (SelfTestFailure, selfTestOutcome) {
	provider := strings.TrimSpace(auth.Provider)
	tester, ok := l.manager.credentialSelfTester(provider)
	if !ok {
		// Not every provider has a credential-check endpoint that is safe to call
		// on a timer; skip it rather than guessing at a probe.
		return SelfTestFailure{}, selfTestOutcomeNotProbed
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
			failure := l.applyProbeResult(parent, CredentialSelfTestResult{
				AuthID:     auth.ID,
				Provider:   provider,
				StatusCode: status,
				Message:    errProbe.Error(),
				ProbeError: true,
			})
			if failure.AuthID != "" {
				return failure, selfTestOutcomeFailed
			}
			return SelfTestFailure{}, selfTestOutcomeProbed
		}
		log.Debugf("credential self-test: probe %s %s failed to run: %v", provider, auth.ID, errProbe)
		return l.recordTransient(auth, errProbe.Error()), selfTestOutcomeFailed
	}
	if result == nil {
		// The executor declined to answer without an error; that is a probe that
		// did not happen, not a credential that passed.
		return SelfTestFailure{}, selfTestOutcomeNotProbed
	}
	if strings.TrimSpace(result.AuthID) == "" {
		result.AuthID = auth.ID
	}
	if strings.TrimSpace(result.Provider) == "" {
		result.Provider = provider
	}
	// The cheap probe only speaks for authorization. If it accepted the
	// credential and this provider can make a real call, follow up with the deep
	// probe on the credentials that need it, so a spent quota is found here rather
	// than by a user request.
	if result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
		if deep, ok := l.deepProbe(ctx, tester, auth); ok {
			deep.AuthID = result.AuthID
			deep.Provider = result.Provider
			failure := l.applyProbeResult(parent, deep)
			if failure.AuthID != "" {
				return failure, selfTestOutcomeFailed
			}
			return SelfTestFailure{}, selfTestOutcomeProbed
		}
	}
	failure := l.applyProbeResult(parent, *result)
	if failure.AuthID != "" {
		return failure, selfTestOutcomeFailed
	}
	return SelfTestFailure{}, selfTestOutcomeProbed
}

// applyProbeResult folds one probe result into scheduling state and stamps the
// tier onto the failure it returns, so the report can tell a rejection only the
// deep probe could see from one the cheap probe would have caught anyway.
func (l *credentialSelfTestLoop) applyProbeResult(ctx context.Context, result CredentialSelfTestResult) SelfTestFailure {
	failure := l.manager.ApplyCredentialSelfTest(ctx, result)
	if failure.AuthID == "" {
		return failure
	}
	tier := result.Tier
	if tier == 0 {
		tier = SelfTestTierAuthorization
	}
	failure.Tier = tier
	return failure
}

// deepProbe runs the generation-level probe when the credential needs one, and
// reports whether it produced a result. It is a no-op when the option is off, when
// the provider cannot make a real call, or when this credential was not selected
// for a deep probe this sweep.
//
// Selection has two parts, and the second is what keeps the pool honest:
//
//   - Every credential the loop had parked — cooling or quarantined — is deep
//     probed on its way back. Those are the ones whose cheap probe just passed
//     while their quota is the thing actually in doubt, so a cheap pass alone
//     would return a spent credential to rotation.
//   - Of the rest, a configured percentage is sampled, so a credential whose
//     quota runs out between recoveries is still found without paying for a real
//     generation call on every healthy credential every sweep.
func (l *credentialSelfTestLoop) deepProbe(ctx context.Context, tester CredentialSelfTester, auth *Auth) (CredentialSelfTestResult, bool) {
	options := l.options
	generator, ok := tester.(CredentialGenerationSelfTester)
	if !ok || !options.DeepProbeEnabled {
		return CredentialSelfTestResult{}, false
	}
	state := auth.SelfTestState()
	returning := state.Strikes > 0 || state.Verdict == SelfTestVerdictCooling || state.Verdict == SelfTestVerdictQuarantine
	if !returning && !l.deepProbeSampled(auth.ID, options.DeepProbeSamplePercent) {
		return CredentialSelfTestResult{}, false
	}
	result, errProbe := generator.SelfTestCredentialGeneration(ctx, auth, options.DeepProbeModel)
	if errProbe != nil {
		// A deep probe that could not run is our problem, not the credential's.
		// The cheap probe already accepted it, so it keeps that verdict rather than
		// being cooled down over a probe failure.
		log.Debugf("credential self-test: deep probe of %s %s failed to run: %v", auth.Provider, auth.ID, errProbe)
		return CredentialSelfTestResult{}, false
	}
	if result == nil {
		return CredentialSelfTestResult{}, false
	}
	return *result, true
}

// deepProbeSampled decides whether a credential is in this sweep's deep-probe
// sample. Selection is a stable hash of the credential id rather than a random
// draw, so the same credentials are picked each sweep: a stable sample spreads
// the deep probe across the pool over successive sweeps, while a random draw
// would re-pick the same credential twice in a row as often as not and leave
// others untouched for long stretches.
func (l *credentialSelfTestLoop) deepProbeSampled(authID string, percent int) bool {
	if percent <= 0 {
		return false
	}
	// The empty-id check comes before the 100% shortcut so the rule holds at every
	// percentage: a credential that cannot be identified is never selected. The
	// loop filters empty ids out before this is reached, so this is defence in
	// depth rather than a live path.
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	if percent >= 100 {
		return true
	}
	hash := fnv.New32a()
	if _, errWrite := hash.Write([]byte(authID)); errWrite != nil {
		return false
	}
	return int(hash.Sum32()%100) < percent
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
