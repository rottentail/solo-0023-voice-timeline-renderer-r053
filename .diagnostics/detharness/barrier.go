package main

// barrierRepo wraps the production storage.Repository and inserts
// deterministic pause points at repository call boundaries.
//
// The two wrapped stacks each own a production Service with its own
// in-process mutex and a distinct Repository instance, exactly mirroring two
// OS processes sharing one data directory. The barriers only choose the
// ordering of repository calls; every storage operation is executed by the
// real production code with unchanged filesystem semantics.

import (
	"fmt"
	"sync"

	"voice-timeline-renderer/internal/domain"
)

type Barrier struct {
	mu     sync.Mutex
	closed map[string]chan struct{}
	trace  *Trace
}

func NewBarrier(trace *Trace) *Barrier {
	return &Barrier{closed: make(map[string]chan struct{}), trace: trace}
}

func (b *Barrier) wait(name string) {
	b.mu.Lock()
	ch, ok := b.closed[name]
	if !ok {
		ch = make(chan struct{})
		b.closed[name] = ch
	}
	b.mu.Unlock()
	b.trace.Eventf("barrier: waiting on %s", name)
	<-ch
	b.trace.Eventf("barrier: passed %s", name)
}

func (b *Barrier) release(name string) {
	b.mu.Lock()
	ch, ok := b.closed[name]
	if !ok {
		ch = make(chan struct{})
		b.closed[name] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
	b.mu.Unlock()
	b.trace.Eventf("barrier: released %s", name)
}

// Rule requests a named barrier before (pre) or after (post) a repository
// method call when the call concerns a matching resource id. Rules are
// assigned per stack, so each instance gets its own gate set. For SaveJob an
// optional MatchState restricts the rule to one render state transition.
type Rule struct {
	Method     string
	MatchID    string
	MatchState string
	PreWait    string
	PostWait   string
}

type barrierRepo struct {
	label   string
	inner   Repo
	rules   []Rule
	trace   *Trace
	barrier *Barrier
}

type Repo interface {
	SaveSource(domain.SourceAsset, []byte) error
	GetSourceMeta(string) (domain.SourceAsset, error)
	GetSource(string) (domain.SourceAsset, []byte, error)
	SaveTimeline(domain.Timeline) error
	GetTimeline(string) (domain.Timeline, error)
	SaveJob(domain.RenderJob) error
	GetJob(string) (domain.RenderJob, error)
	CommitRenderArtifacts(string, []byte, domain.RenderReceipt) error
	GetRenderOutput(string) ([]byte, domain.RenderReceipt, error)
}

func (r *barrierRepo) matching(method, id, state, phase string) string {
	for _, rule := range r.rules {
		if rule.Method != method || rule.MatchID != id {
			continue
		}
		if rule.MatchState != "" && rule.MatchState != state {
			continue
		}
		if phase == "pre" && rule.PreWait != "" {
			return rule.PreWait
		}
		if phase == "post" && rule.PostWait != "" {
			return rule.PostWait
		}
	}
	return ""
}

func (r *barrierRepo) logCall(method, id, phase string, err error) {
	msg := fmt.Sprintf("%s: %s(%s) %s", r.label, method, id, phase)
	if err != nil {
		msg += " -> " + err.Error()
	}
	r.trace.Event(msg)
}

func (r *barrierRepo) SaveTimeline(plan domain.Timeline) error {
	if name := r.matching("SaveTimeline", plan.ID, "", "pre"); name != "" {
		r.barrier.wait(name)
	}
	err := r.inner.SaveTimeline(plan)
	r.logCall("SaveTimeline", plan.ID, "post", err)
	if name := r.matching("SaveTimeline", plan.ID, "", "post"); name != "" {
		r.barrier.wait(name)
	}
	return err
}

func (r *barrierRepo) GetTimeline(id string) (plan domain.Timeline, err error) {
	if name := r.matching("GetTimeline", id, "", "pre"); name != "" {
		r.barrier.wait(name)
	}
	plan, err = r.inner.GetTimeline(id)
	r.logCall("GetTimeline", id, "post", err)
	if name := r.matching("GetTimeline", id, "", "post"); name != "" {
		r.barrier.wait(name)
	}
	return plan, err
}

func (r *barrierRepo) SaveJob(job domain.RenderJob) error {
	if name := r.matching("SaveJob", job.ID, string(job.State), "pre"); name != "" {
		r.barrier.wait(name)
	}
	err := r.inner.SaveJob(job)
	r.logCall("SaveJob", job.ID+" state="+string(job.State), "post", err)
	if name := r.matching("SaveJob", job.ID, string(job.State), "post"); name != "" {
		r.barrier.wait(name)
	}
	return err
}

func (r *barrierRepo) GetJob(id string) (job domain.RenderJob, err error) {
	if name := r.matching("GetJob", id, "", "pre"); name != "" {
		r.barrier.wait(name)
	}
	job, err = r.inner.GetJob(id)
	r.logCall("GetJob", id, "post", err)
	if name := r.matching("GetJob", id, "", "post"); name != "" {
		r.barrier.wait(name)
	}
	return job, err
}

func (r *barrierRepo) CommitRenderArtifacts(id string, wav []byte, receipt domain.RenderReceipt) error {
	if name := r.matching("CommitRenderArtifacts", id, "", "pre"); name != "" {
		r.barrier.wait(name)
	}
	err := r.inner.CommitRenderArtifacts(id, wav, receipt)
	r.logCall("CommitRenderArtifacts", id, "post", err)
	if name := r.matching("CommitRenderArtifacts", id, "", "post"); name != "" {
		r.barrier.wait(name)
	}
	return err
}

func (r *barrierRepo) SaveSource(asset domain.SourceAsset, wav []byte) error {
	if name := r.matching("SaveSource", asset.ID, "", "pre"); name != "" {
		r.barrier.wait(name)
	}
	err := r.inner.SaveSource(asset, wav)
	r.logCall("SaveSource", asset.ID, "post", err)
	if name := r.matching("SaveSource", asset.ID, "", "post"); name != "" {
		r.barrier.wait(name)
	}
	return err
}

func (r *barrierRepo) GetSourceMeta(id string) (domain.SourceAsset, error) {
	return r.inner.GetSourceMeta(id)
}

func (r *barrierRepo) GetSource(id string) (domain.SourceAsset, []byte, error) {
	return r.inner.GetSource(id)
}

func (r *barrierRepo) GetRenderOutput(id string) ([]byte, domain.RenderReceipt, error) {
	return r.inner.GetRenderOutput(id)
}
