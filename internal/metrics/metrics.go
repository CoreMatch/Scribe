package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var histogramBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type authenticationKey struct {
	outcome string
	reason  string
}

type seriesKey struct {
	first  string
	second string
}

type histogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

type Registry struct {
	mu                    sync.Mutex
	authentication        map[authenticationKey]uint64
	authenticationBlocked map[string]uint64
	migrationFailures     map[seriesKey]uint64
	migrations            map[seriesKey]*histogram
	recoveries            map[seriesKey]*histogram
	recoveryScans         map[string]uint64
}

func New() *Registry {
	return &Registry{
		authentication:        make(map[authenticationKey]uint64),
		authenticationBlocked: make(map[string]uint64),
		migrationFailures:     make(map[seriesKey]uint64),
		migrations:            make(map[seriesKey]*histogram),
		recoveries:            make(map[seriesKey]*histogram),
		recoveryScans:         make(map[string]uint64),
	}
}

func (r *Registry) Authentication(outcome, reason string) {
	if r == nil {
		return
	}
	key := authenticationKey{
		outcome: stableLabel(outcome, authenticationOutcomes),
		reason:  stableLabel(reason, authenticationReasons),
	}
	r.mu.Lock()
	r.authentication[key]++
	r.mu.Unlock()
}

func (r *Registry) AuthenticationBlocked(reason string) {
	if r == nil {
		return
	}
	key := stableLabel(reason, blockedReasons)
	r.mu.Lock()
	r.authenticationBlocked[key]++
	r.mu.Unlock()
}

func (r *Registry) MigrationFailed(reason, phase string) {
	if r == nil {
		return
	}
	key := seriesKey{
		first:  stableLabel(reason, migrationFailureReasons),
		second: stableLabel(phase, migrationPhases),
	}
	r.mu.Lock()
	r.migrationFailures[key]++
	r.mu.Unlock()
}

func (r *Registry) Migration(result, phase string, duration time.Duration) {
	if r == nil {
		return
	}
	key := seriesKey{
		first:  stableLabel(result, migrationResults),
		second: stableLabel(phase, migrationPhases),
	}
	r.observe(r.migrations, key, duration)
}

func (r *Registry) Recovery(result, state string, duration time.Duration) {
	if r == nil {
		return
	}
	key := seriesKey{
		first:  stableLabel(result, recoveryResults),
		second: stableLabel(state, migrationStates),
	}
	r.observe(r.recoveries, key, duration)
}

func (r *Registry) RecoveryScan(result string) {
	if r == nil {
		return
	}
	key := stableLabel(result, scanResults)
	r.mu.Lock()
	r.recoveryScans[key]++
	r.mu.Unlock()
}

func (r *Registry) observe(series map[seriesKey]*histogram, key seriesKey, duration time.Duration) {
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	r.mu.Lock()
	value := series[key]
	if value == nil {
		value = &histogram{buckets: make([]uint64, len(histogramBuckets))}
		series[key] = value
	}
	for i, bucket := range histogramBuckets {
		if seconds <= bucket {
			value.buckets[i]++
		}
	}
	value.count++
	value.sum += seconds
	r.mu.Unlock()
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, r.render())
	})
}

func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out strings.Builder
	writeAuthentication(&out, r.authentication)
	writeStringCounter(&out, "scribe_authentication_blocked_total", "Authentication requests blocked by migration or binding failures.", "reason", r.authenticationBlocked)
	writeSeriesCounter(&out, "scribe_migration_failures_total", "Migration failures by stable reason and phase.", r.migrationFailures, "reason", "phase")
	writeHistograms(&out, "scribe_migration_operations_total", "Migration operations by result and phase.", "scribe_migration_duration_seconds", "Migration operation duration in seconds.", r.migrations, "result", "phase")
	writeHistograms(&out, "scribe_recovery_operations_total", "Startup recovery attempts by result and initial state.", "scribe_recovery_duration_seconds", "Startup recovery attempt duration in seconds.", r.recoveries, "result", "state")
	writeStringCounter(&out, "scribe_recovery_scans_total", "Startup migration staging scans by result.", "result", r.recoveryScans)
	return out.String()
}

func writeAuthentication(out *strings.Builder, values map[authenticationKey]uint64) {
	keys := make([]authenticationKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].outcome != keys[j].outcome {
			return keys[i].outcome < keys[j].outcome
		}
		return keys[i].reason < keys[j].reason
	})
	writeHelpType(out, "scribe_authentication_decisions_total", "Authentication race decisions by outcome and reason.", "counter")
	for _, key := range keys {
		fmt.Fprintf(out, "scribe_authentication_decisions_total{outcome=%q,reason=%q} %d\n", key.outcome, key.reason, values[key])
	}
}

func writeStringCounter(out *strings.Builder, name, help, label string, values map[string]uint64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeHelpType(out, name, help, "counter")
	for _, key := range keys {
		fmt.Fprintf(out, "%s{%s=%q} %d\n", name, label, key, values[key])
	}
}

func writeSeriesCounter(out *strings.Builder, name, help string, values map[seriesKey]uint64, firstLabel, secondLabel string) {
	keys := make([]seriesKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].first != keys[j].first {
			return keys[i].first < keys[j].first
		}
		return keys[i].second < keys[j].second
	})
	writeHelpType(out, name, help, "counter")
	for _, key := range keys {
		fmt.Fprintf(out, "%s{%s=%q,%s=%q} %d\n", name, firstLabel, key.first, secondLabel, key.second, values[key])
	}
}

func writeHistograms(out *strings.Builder, counterName, counterHelp, histogramName, histogramHelp string, values map[seriesKey]*histogram, firstLabel, secondLabel string) {
	keys := make([]seriesKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].first != keys[j].first {
			return keys[i].first < keys[j].first
		}
		return keys[i].second < keys[j].second
	})
	writeHelpType(out, counterName, counterHelp, "counter")
	writeHelpType(out, histogramName, histogramHelp, "histogram")
	for _, key := range keys {
		value := values[key]
		fmt.Fprintf(out, "%s{%s=%q,%s=%q} %d\n", counterName, firstLabel, key.first, secondLabel, key.second, value.count)
		for i, bucket := range histogramBuckets {
			fmt.Fprintf(out, "%s_bucket{%s=%q,%s=%q,le=%q} %d\n", histogramName, firstLabel, key.first, secondLabel, key.second, strconv.FormatFloat(bucket, 'g', -1, 64), value.buckets[i])
		}
		fmt.Fprintf(out, "%s_bucket{%s=%q,%s=%q,le=\"+Inf\"} %d\n", histogramName, firstLabel, key.first, secondLabel, key.second, value.count)
		fmt.Fprintf(out, "%s_sum{%s=%q,%s=%q} %s\n", histogramName, firstLabel, key.first, secondLabel, key.second, strconv.FormatFloat(value.sum, 'g', -1, 64))
		fmt.Fprintf(out, "%s_count{%s=%q,%s=%q} %d\n", histogramName, firstLabel, key.first, secondLabel, key.second, value.count)
	}
}

func writeHelpType(out *strings.Builder, name, help, metricType string) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
}

type labelSet map[string]struct{}

var (
	authenticationOutcomes = labelSet{
		"NEW_WINS": {}, "MIGRATE_REUSE_CORE": {}, "MIGRATE_CREATE_CORE": {}, "BLOCK_RETRY": {}, "NO_BINDING": {},
	}
	authenticationReasons = labelSet{
		"none": {}, "target_state_unknown": {}, "migration_intent_required": {}, "migration_opted_out": {},
		"no_playable_legacy_profile": {}, "invalid_credentials": {}, "malformed_target_response": {},
		"malformed_legacy_response": {}, "legacy_unavailable": {}, "authentication_failed": {},
	}
	blockedReasons = labelSet{
		"migration_pending": {}, "migration_state_unknown": {}, "migration_unavailable": {},
		"invalid_legacy_session": {}, "migration_failed": {}, "invalid_migration_result": {},
		"binding_lookup_failed": {}, "permanent_binding_failed": {}, "token_binding_failed": {},
		"refreshed_token_binding_failed": {},
	}
	migrationResults        = labelSet{"success": {}, "failed": {}}
	migrationFailureReasons = labelSet{
		"asset_conflict": {}, "credential_conflict": {}, "profile_conflict": {}, "target_not_empty": {},
		"commit_conflict": {}, "service_auth": {}, "payload_too_large": {}, "response_too_large": {},
		"invalid_request": {}, "invalid_response": {}, "upstream_unavailable": {}, "timeout": {}, "canceled": {},
		"migration_in_progress": {}, "migration_blocked": {}, "snapshot_failed": {}, "staging_failed": {}, "internal": {},
	}
	migrationPhases = labelSet{
		"input": {}, "snapshot": {}, "id": {}, "stage": {}, "validate": {}, "transition": {}, "commit": {}, "bookkeeping": {},
	}
	recoveryResults = labelSet{"success": {}, "blocked": {}}
	scanResults     = labelSet{"success": {}, "failed": {}}
	migrationStates = labelSet{
		"STAGED_IN_REDIS": {}, "APPLYING": {}, "COMMITTED": {}, "LOCKED": {}, "RECONCILING": {},
		"ROLLING_BACK": {}, "ROLLED_BACK": {}, "DISCARDED": {}, "UNKNOWN": {},
	}
)

func stableLabel(value string, allowed labelSet) string {
	if value == "" {
		return "none"
	}
	if _, ok := allowed[value]; !ok {
		return "unknown"
	}
	return value
}
