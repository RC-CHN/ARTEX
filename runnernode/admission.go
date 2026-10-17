package runnernode

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

// Only an explicit ResourceQuota rejection is safe to retry as capacity pressure.
// RBAC Forbidden, admission policy errors and ambiguous create outcomes are not.
var quotaDenied = regexp.MustCompile(`Error from server \(Forbidden\): (?:error when creating "(?:STDIN|-)": )?pods "[a-z0-9.-]+" is forbidden: exceeded quota: ([a-z0-9.-]{1,253}), requested: ([^\n]+), used: ([^\n]+), limited: ([^\n]+)`)
var quotaValue = regexp.MustCompile(`^(count/pods|pods|limits\.(cpu|memory|ephemeral-storage)|requests\.(cpu|memory|ephemeral-storage))=([0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+|[numkKMGTEP]i?)?)$`)

func parseQuotaRejection(err error) *runner.QuotaRejection {
	if err == nil {
		return nil
	}
	m := quotaDenied.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	values := func(raw string) map[string]string {
		out := map[string]string{}
		for _, pair := range strings.Split(raw, ",") {
			if v := quotaValue.FindStringSubmatch(strings.TrimSpace(pair)); v != nil {
				out[v[1]] = v[4]
			}
		}
		return out
	}
	return &runner.QuotaRejection{Name: m[1], Requested: values(m[2]), Used: values(m[3]), Hard: values(m[4])}
}

// The gate covers list+create, but cancellation and backoff never wait on a mutex
// held by another kubectl request. Cleanup does not need this gate.
func (k *KubernetesExecutor) lockCreate(ctx context.Context) error {
	k.createOnce.Do(func() { k.createGate = make(chan struct{}, 1) })
	select {
	case k.createGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			k.unlockCreate()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (k *KubernetesExecutor) unlockCreate() { <-k.createGate }

func (k *KubernetesExecutor) admissionStage(id, stage string, attempt bool) {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	if k.admissionWaiting == nil {
		k.admissionWaiting = map[string]runner.AdmissionWait{}
	}
	w := k.admissionWaiting[id]
	// A retry's short create RPC must not hide the capacity wait at the acquire
	// deadline. Success replaces this with pod_ready before tool initialization.
	if stage == "pod_create" && w.Stage == "quota" {
		stage = "quota"
	}
	changed := w.Stage != stage || attempt
	if w.SessionID == "" {
		w.Since = time.Now()
	}
	w.SessionID, w.Stage = id, stage
	if attempt {
		w.Attempts++
	}
	k.admissionWaiting[id] = w
	if changed {
		k.notifyAdmissionLocked()
	}
}
func (k *KubernetesExecutor) finishAdmission(id string) {
	k.admissionMu.Lock()
	delete(k.admissionWaiting, id)
	k.notifyAdmissionLocked()
	k.admissionMu.Unlock()
}
func (k *KubernetesExecutor) admissionDelay() time.Duration {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	return time.Until(k.admission.RetryAt)
}
func (k *KubernetesExecutor) recordQuotaRejection(q *runner.QuotaRejection) {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	now := time.Now()
	q.At = now
	k.admission.QuotaRejections++
	k.quotaFailures = min(k.quotaFailures+1, 6)
	k.admission.LastQuotaRejection = q
	delay := min(250*time.Millisecond*time.Duration(1<<min(k.quotaFailures, 5)), 5*time.Second)
	k.admission.RetryAt = now.Add(delay)
	if k.Log != nil && (k.quotaFailures == 1 || now.Sub(k.admissionLogAt) >= 30*time.Second) {
		b, _ := json.Marshal(q)
		fmt.Fprintf(k.Log, "runner_admission event=quota_wait rejections=%d retry_ms=%d quota=%s\n", k.admission.QuotaRejections, delay.Milliseconds(), b)
		k.admissionLogAt = now
	}
}
func (k *KubernetesExecutor) recordPodCreated() {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	if k.quotaFailures > 0 && k.Log != nil {
		fmt.Fprintf(k.Log, "runner_admission event=quota_recovered rejections=%d\n", k.admission.QuotaRejections)
	}
	k.quotaFailures = 0
	k.admission.RetryAt = time.Time{}
	k.admission.LastCreateAt = time.Now()
}
func (k *KubernetesExecutor) admissionStatus() *runner.AdmissionStatus {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	s := k.admission
	s.ObservedAt = time.Now()
	s.Waiting = make([]runner.AdmissionWait, 0, len(k.admissionWaiting))
	for _, w := range k.admissionWaiting {
		s.Waiting = append(s.Waiting, w)
	}
	sort.Slice(s.Waiting, func(i, j int) bool { return s.Waiting[i].SessionID < s.Waiting[j].SessionID })
	if s.LastQuotaRejection != nil {
		q := *s.LastQuotaRejection
		q.Requested, q.Used, q.Hard = maps.Clone(q.Requested), maps.Clone(q.Used), maps.Clone(q.Hard)
		s.LastQuotaRejection = &q
	}
	return &s
}
func waitAdmission(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (k *KubernetesExecutor) admissionChanges() <-chan struct{} {
	k.admissionMu.Lock()
	defer k.admissionMu.Unlock()
	if k.admissionEvents == nil {
		k.admissionEvents = make(chan struct{}, 1)
	}
	return k.admissionEvents
}
func (k *KubernetesExecutor) notifyAdmissionLocked() {
	if k.admissionEvents != nil {
		select {
		case k.admissionEvents <- struct{}{}:
		default:
		}
	}
}
