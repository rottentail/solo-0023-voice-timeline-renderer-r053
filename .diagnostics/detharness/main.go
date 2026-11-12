package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

type Stack struct {
	label   string
	repo    *barrierRepo
	service *service.Service
}

type ScenarioResult struct {
	Scenario string         `json:"scenario"`
	Purpose  string         `json:"purpose"`
	Outcomes map[string]any `json:"outcomes"`
	Files    map[string]any `json:"files_on_disk"`
	Verdict  string         `json:"verdict"`
	Details  []string       `json:"details"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: detharness <scenario> <output-dir>")
		os.Exit(2)
	}
	scenario := os.Args[1]
	outDir := os.Args[2]
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		fatal(err)
	}
	dataDir := filepath.Join(outDir, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		fatal(err)
	}

	result := runScenario(scenario, dataDir, outDir)
	writeJSONFile(filepath.Join(outDir, "result.json"), result)
	fmt.Println("VERDICT:", result.Verdict)
	for _, detail := range result.Details {
		fmt.Println(" -", detail)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}

func newStack(label, dataDir string, trace *Trace, barrier *Barrier, rules []Rule) *Stack {
	repo, err := storage.NewRepository(dataDir)
	if err != nil {
		fatal(err)
	}
	wrapped := &barrierRepo{label: label, inner: repo, rules: rules, trace: trace, barrier: barrier}
	svc := service.New(wrapped, service.DefaultConfig())
	return &Stack{label: label, repo: wrapped, service: svc}
}

// ---- fixtures -------------------------------------------------------------

func makeWAV(seed byte, frames int, sampleRate int) []byte {
	channels := 1
	bytesPerSample := 2
	dataBytes := frames * channels * bytesPerSample
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*channels*bytesPerSample))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(channels*bytesPerSample))
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	for i := 0; i < frames; i++ {
		v := int16(int(seed)*97 + i*13)
		binary.LittleEndian.PutUint16(buf[44+i*2:46+i*2], uint16(v))
	}
	return buf
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func registerSource(ctx context.Context, st *Stack, id string, wav []byte) (domain.SourceAsset, bool, error) {
	return st.service.CreateSource(ctx, id, service.SourceWriteRequest{
		Name:      "src-" + id,
		WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
}

func timelineReq(name, sourceID string, frameCount int64) service.TimelineWriteRequest {
	return service.TimelineWriteRequest{
		Name:           name,
		SampleRate:     16000,
		OutputChannels: 1,
		TargetPeakDBFS: -3,
		Spans: []domain.TimelineSpan{{
			SourceID:         sourceID,
			SourceStartFrame: 0,
			SourceEndFrame:   frameCount,
			GainDB:           0,
			CrossfadeFrames:  0,
		}},
	}
}

func errCode(err error) map[string]any {
	if err == nil {
		return nil
	}
	app := domain.AsAppError(err)
	return map[string]any{
		"kind":    string(app.Kind),
		"code":    app.Detail.Code,
		"field":   app.Detail.Field,
		"message": app.Detail.Message,
	}
}

func writeJSONFile(path string, value any) {
	payload, _ := json.MarshalIndent(value, "", "  ")
	_ = os.WriteFile(path, append(payload, '\n'), 0o640)
}

func readJSONFile(path string, target any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

// snapshotDisk captures the durable content each caller can later read back.
func snapshotDisk(dataDir string) map[string]any {
	snapshot := map[string]any{}
	for _, kind := range []string{"timelines", "jobs"} {
		entries, _ := os.ReadDir(filepath.Join(dataDir, kind))
		items := map[string]any{}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			var raw map[string]any
			if err := readJSONFile(filepath.Join(dataDir, kind, entry.Name()), &raw); err == nil {
				items[entry.Name()] = raw
			}
		}
		snapshot[kind] = items
	}
	rendersDir := filepath.Join(dataDir, "renders")
	renderEntries, _ := os.ReadDir(rendersDir)
	renderItems := map[string]any{}
	for _, entry := range renderEntries {
		receiptPath := filepath.Join(rendersDir, entry.Name(), "receipt.json")
		var receipt map[string]any
		if err := readJSONFile(receiptPath, &receipt); err == nil {
			outputPath := filepath.Join(rendersDir, entry.Name(), "output.wav")
			if payload, readErr := os.ReadFile(outputPath); readErr == nil {
				receipt["output_digest_on_disk"] = digestOf(payload)
			}
			renderItems[entry.Name()] = receipt
		} else {
			renderItems[entry.Name()] = "no receipt: " + err.Error()
		}
	}
	snapshot["renders"] = renderItems
	sourcesDir := filepath.Join(dataDir, "sources")
	sourceEntries, _ := os.ReadDir(sourcesDir)
	sourceItems := map[string]any{}
	for _, entry := range sourceEntries {
		mediaPath := filepath.Join(sourcesDir, entry.Name(), "source.wav")
		if payload, err := os.ReadFile(mediaPath); err == nil {
			sourceItems[entry.Name()] = digestOf(payload)
		}
	}
	snapshot["sources_digest"] = sourceItems
	return snapshot
}

type callOutcome struct {
	Status string         `json:"status"`
	Body   map[string]any `json:"body,omitempty"`
	Error  map[string]any `json:"error,omitempty"`
}

func timelineOutcome(plan domain.Timeline, created bool, err error) callOutcome {
	out := callOutcome{}
	if err != nil {
		out.Status = "error"
		out.Error = errCode(err)
		return out
	}
	if created {
		out.Status = "201_created"
	} else {
		out.Status = "200_idempotent_return"
	}
	raw, _ := json.Marshal(plan)
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func jobOutcome(job domain.RenderJob, created bool, err error) callOutcome {
	out := callOutcome{}
	if err != nil {
		out.Status = "error"
		out.Error = errCode(err)
		if job.ID != "" {
			raw, _ := json.Marshal(job)
			_ = json.Unmarshal(raw, &out.Body)
		}
		return out
	}
	if created {
		out.Status = "201_created"
	} else {
		out.Status = "200_idempotent_return"
	}
	raw, _ := json.Marshal(job)
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func runScenario(name, dataDir, outDir string) ScenarioResult {
	trace := NewTrace(outDir)
	barrier := NewBarrier(trace)
	ctx := context.Background()

	switch name {
	case "S1-storage-timeline-primitive":
		return scenarioStoragePrimitive(outDir, trace, barrier)
	case "S2-storage-directory-guard":
		return scenarioDirectoryGuard(ctx, dataDir, outDir, trace, barrier)
	case "T1-race-timeline-overwrite":
		return scenarioTimelineRace(ctx, dataDir, outDir, trace, barrier)
	case "T2-sequential-conflict":
		return scenarioSequentialConflict(ctx, dataDir, outDir, trace)
	case "T3-idempotent-retry":
		return scenarioIdempotentRetry(ctx, dataDir, outDir, trace)
	case "T4-distinct-ids-control":
		return scenarioDistinctControl(ctx, dataDir, outDir, trace)
	case "T5-source-same-id-race":
		return scenarioSourceRace(ctx, dataDir, outDir, trace)
	case "R1-race-render-same-id":
		return scenarioRenderRace(ctx, dataDir, outDir, trace, barrier, false)
	case "R2-race-render-same-id-different-timelines":
		return scenarioRenderRace(ctx, dataDir, outDir, trace, barrier, true)
	case "R3-distinct-render-ids-control":
		return scenarioRenderDistinctControl(ctx, dataDir, outDir, trace)
	case "R4-sequential-render-retry-conflict":
		return scenarioRenderSequential(ctx, dataDir, outDir, trace, barrier)
	default:
		fatal(fmt.Errorf("unknown scenario %q", name))
		return ScenarioResult{}
	}
}

func setupTwoSources(ctx context.Context, dataDir string, trace *Trace, barrier *Barrier) (string, string, []byte, []byte) {
	bootstrap := newStack("bootstrap", dataDir, trace, barrier, nil)
	wavX := makeWAV(1, 480, 16000)
	wavY := makeWAV(7, 480, 16000)
	if _, _, err := registerSource(ctx, bootstrap, "src-x", wavX); err != nil {
		fatal(err)
	}
	if _, _, err := registerSource(ctx, bootstrap, "src-y", wavY); err != nil {
		fatal(err)
	}
	return "src-x", "src-y", wavX, wavY
}

// ---- storage-level root cause ---------------------------------------------

// modeledJSONSave reproduces the exact algorithm of storage.SaveTimeline +
// storage.writeJSONAtomic (stat guard, temp file, fsync, rename, dir fsync)
// using the same os calls, with a deterministic barrier between the stat and
// the rename. This is a faithful model of the production algorithm in
// internal/storage/timelines.go and internal/storage/atomic.go, used to make
// the irreducible stat->rename window observable without modifying code.
func modeledJSONSave(dir, path string, payload []byte, postStatBarrier *Barrier) error {
	if _, err := os.Stat(path); err == nil {
		return errors.New("modeled: identifier already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if postStatBarrier != nil {
		postStatBarrier.wait("stat-done")
	}
	temp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func scenarioStoragePrimitive(outDir string, trace *Trace, barrier *Barrier) ScenarioResult {
	dir := filepath.Join(outDir, "data", "timelines")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fatal(err)
	}
	path := filepath.Join(dir, "tl-model.json")
	type saveResult struct {
		who string
		err error
	}
	ch := make(chan saveResult, 2)
	trace.Event("S1: both writers perform existence stat while the file is absent, then both proceed to rename")
	go func() { ch <- saveResult{"A", modeledJSONSave(dir, path, []byte("{\"content\":\"PLAN-A\"}\n"), barrier)} }()
	go func() { ch <- saveResult{"B", modeledJSONSave(dir, path, []byte("{\"content\":\"PLAN-B\"}\n"), barrier)} }()
	waitForTraceCount(trace, "barrier: waiting on stat-done", 2)
	trace.Event("orchestrator: both existence stats returned NotFound; releasing both renames")
	barrier.release("stat-done")
	results := map[string]any{}
	for i := 0; i < 2; i++ {
		r := <-ch
		entry := map[string]any{"returned_error": fmt.Sprint(r.err)}
		results["writer-"+r.who] = entry
	}
	finalBytes, _ := os.ReadFile(path)
	_ = trace.writeFile("interleaving-trace.txt")

	result := ScenarioResult{
		Scenario: "S1-storage-timeline-primitive",
		Purpose:  "faithful model of the production SaveTimeline algorithm: both stat checks miss, then two atomic renames target one path",
		Outcomes: results,
		Files:    map[string]any{"tl-model.json": string(finalBytes)},
		Details: []string{
			"production reference: internal/storage/timelines.go (stat guard) + internal/storage/atomic.go writeJSONAtomic (temp+rename)",
			"POSIX file rename over an existing path succeeds and replaces the target atomically; there is no O_EXCL on rename",
		},
	}
	noErrors := true
	for _, raw := range results {
		if raw.(map[string]any)["returned_error"] != "<nil>" {
			noErrors = false
		}
	}
	finalText := string(finalBytes)
	if noErrors && (strings.Contains(finalText, "PLAN-A") || strings.Contains(finalText, "PLAN-B")) {
		result.Verdict = "ROOT_CAUSE_CONFIRMED: both writers returned success; only one content binding survives on disk (here: " + strings.TrimSpace(finalText) + ") so the other writer's successful create commitment was silently replaced"
	} else {
		result.Verdict = "UNEXPECTED: inspect trace"
	}
	return result
}

func scenarioDirectoryGuard(ctx context.Context, dataDir, outDir string, trace *Trace, barrier *Barrier) ScenarioResult {
	// Real production repositories, sequential calls: the directory-based
	// commit path used by sources and render artifacts.
	repoA, err := storage.NewRepository(dataDir)
	if err != nil {
		fatal(err)
	}
	repoB, err := storage.NewRepository(dataDir)
	if err != nil {
		fatal(err)
	}
	wavP := makeWAV(3, 320, 16000)
	assetP := domain.SourceAsset{ID: "src-guard", Name: "src-guard", Digest: digestOf(wavP), SampleRate: 16000, Channels: 1, BitsPerSample: 16, FrameCount: 320, ByteCount: int64(len(wavP))}
	firstErr := repoA.SaveSource(assetP, wavP)
	secondErr := repoB.SaveSource(assetP, wavP)

	// Model the concurrent both-stat-miss case for directory commits.
	modelDir := filepath.Join(outDir, "model")
	if err := os.MkdirAll(filepath.Join(modelDir, "sources"), 0o750); err != nil {
		fatal(err)
	}
	final := filepath.Join(modelDir, "sources", "src-model")
	dirSave := func(who, marker string, ch chan<- map[string]any) {
		if _, err := os.Stat(final); err == nil {
			ch <- map[string]any{"who": who, "err": "modeled: identifier already exists"}
			return
		}
		barrier.wait("dir-stat-done")
		temp, err := os.MkdirTemp(filepath.Join(modelDir, "sources"), "source-*")
		if err != nil {
			ch <- map[string]any{"who": who, "err": err.Error()}
			return
		}
		if err := os.WriteFile(filepath.Join(temp, "marker.txt"), []byte(marker), 0o640); err != nil {
			ch <- map[string]any{"who": who, "err": err.Error()}
			return
		}
		renameErr := os.Rename(temp, final)
		ch <- map[string]any{"who": who, "err": fmt.Sprint(renameErr)}
	}
	modelCh := make(chan map[string]any, 2)
	go dirSave("A", "A-content", modelCh)
	go dirSave("B", "B-content", modelCh)
	waitForTraceCount(trace, "barrier: waiting on dir-stat-done", 2)
	barrier.release("dir-stat-done")
	modelResults := []map[string]any{}
	for i := 0; i < 2; i++ {
		modelResults = append(modelResults, <-modelCh)
	}
	_ = trace.writeFile("interleaving-trace.txt")

	result := ScenarioResult{
		Scenario: "S2-storage-directory-guard",
		Purpose:  "directory commits (sources, render artifacts): sequential conflict plus concurrent both-stat-miss ENOTEMPTY behavior",
		Outcomes: map[string]any{
			"real_repo_sequential_first_save":  fmt.Sprint(firstErr),
			"real_repo_sequential_second_save": errCode(secondErr),
			"model_concurrent_dir_rename":      modelResults,
		},
		Files: snapshotDisk(dataDir),
		Details: []string{
			"production reference: internal/storage/sources.go SaveSource and renders.go CommitRenderArtifacts rename a non-empty staging directory into place",
			"on Linux renameat of a non-empty directory over an existing non-empty directory fails with ENOTEMPTY",
		},
	}
	guardSeen := false
	for _, entry := range modelResults {
		text := entry["err"].(string)
		if strings.Contains(text, "directory not empty") ||
			strings.Contains(text, "ENOTEMPTY") ||
			strings.Contains(text, "file exists") ||
			strings.Contains(text, "EEXIST") {
			guardSeen = true
		}
	}
	sequentialConflict := secondErr != nil && errCode(secondErr)["code"] == "source_conflict"
	if firstErr == nil && sequentialConflict && guardSeen {
		result.Verdict = "GUARD_CONFIRMED: directory rename prevents silent overwrite (concurrent loser gets a kernel-level ENOTEMPTY/EEXIST, surfaced as a 500 storage_error); sequential duplicates still get the clean 409 source_conflict"
	} else {
		result.Verdict = "UNEXPECTED: inspect trace"
	}
	return result
}

// ---- timeline scenarios ---------------------------------------------------

func scenarioTimelineRace(ctx context.Context, dataDir, outDir string, trace *Trace, barrier *Barrier) ScenarioResult {
	srcX, srcY, wavX, wavY := setupTwoSources(ctx, dataDir, trace, barrier)
	tlID := "tl-contended"
	// Service-level TOCTOU: B's GetTimeline (the service existence check)
	// returns NotFound and is held until A's whole create is durable. B then
	// continues into the storage layer with stale NotFound knowledge.
	rulesB := []Rule{{Method: "GetTimeline", MatchID: tlID, PostWait: "b-get-tl"}}
	a := newStack("instance-A", dataDir, trace, barrier, nil)
	b := newStack("instance-B", dataDir, trace, barrier, rulesB)

	outcomes := map[string]callOutcome{}
	type response struct {
		who     string
		plan    domain.Timeline
		created bool
		err     error
	}
	respCh := make(chan response, 2)
	trace.Event("scenario T1: launching both CreateTimeline calls for " + tlID)
	go func() {
		plan, created, err := a.service.CreateTimeline(ctx, tlID, timelineReq("plan-X", srcX, 480))
		respCh <- response{"A", plan, created, err}
	}()
	go func() {
		plan, created, err := b.service.CreateTimeline(ctx, tlID, timelineReq("plan-Y", srcY, 480))
		respCh <- response{"B", plan, created, err}
	}()

	waitForTrace(trace, "instance-A: SaveTimeline("+tlID+") post")
	trace.Event("orchestrator: A timeline durable; releasing B after its stale NotFound check")
	barrier.release("b-get-tl")

	for i := 0; i < 2; i++ {
		r := <-respCh
		trace.Eventf("response %s: created=%v err=%v", r.who, r.created, r.err)
		outcomes["call-"+r.who] = timelineOutcome(r.plan, r.created, r.err)
	}
	_ = trace.writeFile("interleaving-trace.txt")

	var onDisk domain.Timeline
	diskErr := readJSONFile(filepath.Join(dataDir, "timelines", tlID+".json"), &onDisk)
	result := ScenarioResult{
		Scenario: "T1-race-timeline-overwrite",
		Purpose:  "two instances create the same timeline_id with different content; B's service existence check is stale by construction",
		Outcomes: map[string]any{"responses": outcomes},
		Files:    snapshotDisk(dataDir),
		Details: []string{
			"wav digest src-x=" + digestOf(wavX),
			"wav digest src-y=" + digestOf(wavY),
			"final timeline file read error: " + fmt.Sprint(diskErr),
		},
	}
	if diskErr == nil {
		result.Details = append(result.Details,
			"final durable timeline name="+onDisk.Name+" span source="+onDisk.Spans[0].SourceID,
			"final CreatedAt="+onDisk.CreatedAt.String())
	}
	bOut := outcomes["call-B"]
	aOut := outcomes["call-A"]
	if aOut.Status == "201_created" && bOut.Status == "error" && bOut.Error["code"] == "timeline_conflict" {
		result.Verdict = "CONFLICT_CAUGHT_AT_STORAGE: A committed first; B's storage-level stat guard rejected the stale create (timing-dependent: an earlier B storage entry would instead silently overwrite, as proven deterministically by S1 and naturally by the dual-process run)"
	} else if aOut.Status == "201_created" && bOut.Status == "201_created" {
		result.Verdict = "COMMITMENT_OVERWRITTEN: both callers received 201 Created for different content bound to one id"
	} else {
		result.Verdict = "UNEXPECTED: inspect trace"
	}
	return result
}

func scenarioSequentialConflict(ctx context.Context, dataDir, outDir string, trace *Trace) ScenarioResult {
	srcX, srcY, _, _ := setupTwoSources(ctx, dataDir, trace, NewBarrier(trace))
	tlID := "tl-sequential"
	a := newStack("instance-A", dataDir, trace, NewBarrier(trace), nil)
	b := newStack("instance-B", dataDir, trace, NewBarrier(trace), nil)

	planA, createdA, errA := a.service.CreateTimeline(ctx, tlID, timelineReq("plan-X", srcX, 480))
	outA := timelineOutcome(planA, createdA, errA)
	planB, createdB, errB := b.service.CreateTimeline(ctx, tlID, timelineReq("plan-Y", srcY, 480))
	outB := timelineOutcome(planB, createdB, errB)
	planB2, createdB2, errB2 := b.service.CreateTimeline(ctx, tlID, timelineReq("plan-X", srcX, 480))
	outB2 := timelineOutcome(planB2, createdB2, errB2)
	_ = trace.writeFile("interleaving-trace.txt")

	result := ScenarioResult{
		Scenario: "T2-sequential-conflict",
		Purpose:  "non-overlapping calls: explicit conflict and idempotent retry semantics across processes",
		Outcomes: map[string]any{
			"A-first-create":      outA,
			"B-different-content": outB,
			"B-same-content":      outB2,
		},
		Files: snapshotDisk(dataDir),
		Details: []string{
			"establishes the baseline: conflict detection works whenever calls do not overlap in the stat->rename window",
		},
	}
	if outB.Status == "error" && outB.Error["code"] == "timeline_conflict" &&
		outB2.Status == "200_idempotent_return" && outA.Status == "201_created" {
		result.Verdict = "EXPECTED_SEMANTICS_PRESERVED: conflict (409) for different content, idempotent 200 for identical retry"
	} else {
		result.Verdict = "UNEXPECTED: sequential semantics diverged from contract"
	}
	return result
}

func scenarioIdempotentRetry(ctx context.Context, dataDir, outDir string, trace *Trace) ScenarioResult {
	srcX, _, _, _ := setupTwoSources(ctx, dataDir, trace, NewBarrier(trace))
	tlID := "tl-retry"
	a := newStack("instance-A", dataDir, trace, NewBarrier(trace), nil)
	b := newStack("instance-B", dataDir, trace, NewBarrier(trace), nil)
	planA, cA, eA := a.service.CreateTimeline(ctx, tlID, timelineReq("plan-X", srcX, 480))
	planB, cB, eB := b.service.CreateTimeline(ctx, tlID, timelineReq("plan-X", srcX, 480))
	_ = trace.writeFile("interleaving-trace.txt")
	result := ScenarioResult{
		Scenario: "T3-idempotent-retry",
		Purpose:  "identical content submitted to the second instance is a normal retry, not a conflict",
		Outcomes: map[string]any{
			"A": timelineOutcome(planA, cA, eA),
			"B": timelineOutcome(planB, cB, eB),
		},
		Files: snapshotDisk(dataDir),
	}
	if cA && !cB && eB == nil && planB.Spans[0].SourceID == srcX {
		result.Verdict = "RETRY_OK: identical request returns the stored binding with 200, no overwrite"
	} else {
		result.Verdict = "UNEXPECTED: retry semantics broken"
	}
	return result
}

func scenarioDistinctControl(ctx context.Context, dataDir, outDir string, trace *Trace) ScenarioResult {
	srcX, srcY, _, _ := setupTwoSources(ctx, dataDir, trace, NewBarrier(trace))
	a := newStack("instance-A", dataDir, trace, NewBarrier(trace), nil)
	b := newStack("instance-B", dataDir, trace, NewBarrier(trace), nil)
	type pair struct {
		who     string
		plan    domain.Timeline
		created bool
		err     error
	}
	ch := make(chan pair, 2)
	go func() {
		p, c, e := a.service.CreateTimeline(ctx, "tl-alpha", timelineReq("alpha", srcX, 480))
		ch <- pair{"A", p, c, e}
	}()
	go func() {
		p, c, e := b.service.CreateTimeline(ctx, "tl-beta", timelineReq("beta", srcY, 480))
		ch <- pair{"B", p, c, e}
	}()
	outcomes := map[string]any{}
	for i := 0; i < 2; i++ {
		r := <-ch
		outcomes["call-"+r.who] = timelineOutcome(r.plan, r.created, r.err)
	}
	_ = trace.writeFile("interleaving-trace.txt")
	result := ScenarioResult{
		Scenario: "T4-distinct-ids-control",
		Purpose:  "parallel registration of different identifiers (the legal control case)",
		Outcomes: map[string]any{"responses": outcomes},
		Files:    snapshotDisk(dataDir),
	}
	ok := outcomes["call-A"].(callOutcome).Status == "201_created" &&
		outcomes["call-B"].(callOutcome).Status == "201_created"
	if ok {
		result.Verdict = "CONTROL_OK: distinct identifiers committed independently and both survive"
	} else {
		result.Verdict = "CONTROL_FAILED"
	}
	return result
}

func scenarioSourceRace(ctx context.Context, dataDir, outDir string, trace *Trace) ScenarioResult {
	wavP := makeWAV(3, 320, 16000)
	wavQ := makeWAV(9, 320, 16000)
	a := newStack("instance-A", dataDir, trace, NewBarrier(trace), nil)
	b := newStack("instance-B", dataDir, trace, NewBarrier(trace), nil)
	type pair struct {
		who     string
		asset   domain.SourceAsset
		created bool
		err     error
	}
	ch := make(chan pair, 2)
	go func() {
		asset, created, err := registerSource(ctx, a, "src-contended", wavP)
		ch <- pair{"A", asset, created, err}
	}()
	go func() {
		asset, created, err := registerSource(ctx, b, "src-contended", wavQ)
		ch <- pair{"B", asset, created, err}
	}()
	outcomes := map[string]any{}
	for i := 0; i < 2; i++ {
		r := <-ch
		out := callOutcome{}
		if r.err != nil {
			out.Status = "error"
			out.Error = errCode(r.err)
		} else if r.created {
			out.Status = "201_created"
		} else {
			out.Status = "200_idempotent_return"
		}
		outcomes["call-"+r.who] = out
	}
	_ = trace.writeFile("interleaving-trace.txt")
	result := ScenarioResult{
		Scenario: "T5-source-same-id-race",
		Purpose:  "same source_id created concurrently: directory rename (ENOTEMPTY) is the cross-process guard",
		Outcomes: map[string]any{"responses": outcomes},
		Files:    snapshotDisk(dataDir),
		Details: []string{
			"src digest candidate P=" + digestOf(wavP),
			"src digest candidate Q=" + digestOf(wavQ),
		},
	}
	errors := 0
	for _, raw := range outcomes {
		if raw.(callOutcome).Status == "error" {
			errors++
		}
	}
	if errors == 1 {
		result.Verdict = "GUARDED_BY_DIRECTORY_RENAME: exactly one caller succeeded; the loser failed instead of overwriting (surfaced as storage_error, not a clean 409 conflict)"
	} else {
		result.Verdict = "UNEXPECTED: inspect trace"
	}
	return result
}

// ---- render scenarios -----------------------------------------------------
func scenarioRenderRace(ctx context.Context, dataDir, outDir string, trace *Trace, barrier *Barrier, differentTimelines bool) ScenarioResult {
	srcX, srcY, wavX, wavY := setupTwoSources(ctx, dataDir, trace, barrier)
	bootstrap := newStack("bootstrap2", dataDir, trace, barrier, nil)
	tlA, tlB := "tl-render-x", "tl-render-y"
	if _, _, err := bootstrap.service.CreateTimeline(ctx, tlA, timelineReq("plan-X", srcX, 480)); err != nil {
		fatal(err)
	}
	if _, _, err := bootstrap.service.CreateTimeline(ctx, tlB, timelineReq("plan-Y", srcY, 480)); err != nil {
		fatal(err)
	}
	renderID := "rnd-contended"
	timelineForB := tlA
	if differentTimelines {
		timelineForB = tlB
	}

	// Deterministic dangerous interleaving:
	//   1. Both instances' GetJob existence checks return NotFound (gated
	//      until both have executed them).
	//   2. A runs its whole pipeline first and reaches the completed job
	//      file; B stays paused before its queued SaveJob.
	//   3. B then runs the full pipeline against its own timeline and its
	//      job file writes / artifact commit collide with A's commitment.
	rulesA := []Rule{
		{Method: "GetJob", MatchID: renderID, PostWait: "g1-existence"},
	}
	rulesB := []Rule{
		{Method: "GetJob", MatchID: renderID, PostWait: "g1-existence"},
		{Method: "SaveJob", MatchID: renderID, MatchState: string(domain.RenderQueued), PreWait: "g2-a-done"},
	}
	a2 := newStack("instance-A", dataDir, trace, barrier, rulesA)
	b2 := newStack("instance-B", dataDir, trace, barrier, rulesB)

	type pair struct {
		who     string
		job     domain.RenderJob
		created bool
		err     error
	}
	ch := make(chan pair, 2)
	trace.Event("scenario render race: launching both CreateRender for " + renderID)
	go func() {
		job, created, err := a2.service.CreateRender(ctx, renderID, service.RenderWriteRequest{TimelineID: tlA})
		ch <- pair{"A", job, created, err}
	}()
	go func() {
		job, created, err := b2.service.CreateRender(ctx, renderID, service.RenderWriteRequest{TimelineID: timelineForB})
		ch <- pair{"B", job, created, err}
	}()

	waitForTraceCount(trace, "GetJob("+renderID+") post", 2)
	trace.Event("orchestrator: both GetJob existence checks returned NotFound; releasing A to create the job")
	barrier.release("g1-existence")
	waitForTrace(trace, "instance-A: SaveJob("+renderID+" state=completed) post")
	trace.Event("orchestrator: A reached completed; releasing B's stale pipeline")
	barrier.release("g2-a-done")

	outcomes := map[string]callOutcome{}
	for i := 0; i < 2; i++ {
		r := <-ch
		trace.Eventf("render response %s: created=%v state=%s err=%v", r.who, r.created, r.job.State, r.err)
		outcomes["call-"+r.who] = jobOutcome(r.job, r.created, r.err)
	}
	_ = trace.writeFile("interleaving-trace.txt")

	readback := map[string]any{}
	if job, err := a2.service.GetRender(ctx, renderID); err == nil {
		raw, _ := json.Marshal(job)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		readback["job-via-A"] = m
	} else {
		readback["job-via-A"] = errCode(err)
	}
	if job, err := b2.service.GetRender(ctx, renderID); err == nil {
		raw, _ := json.Marshal(job)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		readback["job-via-B"] = m
	} else {
		readback["job-via-B"] = errCode(err)
	}
	outputObservation := map[string]any{}
	if payload, receipt, err := a2.service.GetOutput(ctx, renderID); err == nil {
		raw, _ := json.Marshal(receipt)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["output_digest_observed"] = digestOf(payload)
		outputObservation["receipt_and_output"] = m
	} else {
		outputObservation["error"] = errCode(err)
	}

	scenarioID := "R1-race-render-same-id"
	purpose := "both existence checks miss; A completes first, B's stale pipeline then collides with the durable render"
	if differentTimelines {
		scenarioID = "R2-race-render-same-id-different-timelines"
		purpose = "same render_id, each caller binds a different timeline; both existence checks miss then B commits late"
	}
	result := ScenarioResult{
		Scenario: scenarioID,
		Purpose:  purpose,
		Outcomes: map[string]any{
			"responses":         outcomes,
			"readback":          readback,
			"output_observable": outputObservation,
			"digest_src_x":      digestOf(wavX),
			"digest_src_y":      digestOf(wavY),
			"timeline_a":        tlA,
			"timeline_b":        timelineForB,
		},
		Files: snapshotDisk(dataDir),
	}

	finalJob, finalErr := a2.service.GetRender(ctx, renderID)
	violations := []string{}
	if finalErr == nil {
		result.Details = append(result.Details, "final durable job state="+string(finalJob.State)+" timeline="+finalJob.TimelineID)
		outA, hasA := outcomes["call-A"]
		outB, hasB := outcomes["call-B"]
		if hasA && outA.Status == "201_created" {
			stateA, _ := outA.Body["state"].(string)
			result.Details = append(result.Details, "caller A was answered with state="+stateA)
			if stateA == string(domain.RenderCompleted) && finalJob.State != domain.RenderCompleted {
				violations = append(violations, "caller A received a completed commitment that the shared job file no longer represents: final state is "+string(finalJob.State))
			}
		}
		if hasB && outB.Status == "error" {
			violations = append(violations, "caller B received a 409 ("+fmt.Sprint(outB.Error["code"])+") from CommitRenderArtifacts; failRender then overwrote the shared job file with a failed terminal record")
		}
		if finalJob.State == domain.RenderFailed {
			violations = append(violations, "terminal failed job while renders/"+renderID+" artifacts remain published; receipt and output endpoints now deny an existing immutable artifact")
		}
		if differentTimelines && finalJob.TimelineID != timelineForB {
			violations = append(violations, "the surviving job binding does not match every caller's promised timeline")
		}
	}
	if len(violations) > 0 {
		result.Verdict = "COMMITMENT_CONTRADICTION: " + strings.Join(violations, "; ")
	} else {
		result.Verdict = "NO_CONTRADICTION_OBSERVED: inspect trace"
	}
	return result
}

func scenarioRenderSequential(ctx context.Context, dataDir, outDir string, trace *Trace, barrier *Barrier) ScenarioResult {
	srcX, srcY, _, _ := setupTwoSources(ctx, dataDir, trace, barrier)
	a := newStack("instance-A", dataDir, trace, barrier, nil)
	b := newStack("instance-B", dataDir, trace, barrier, nil)
	if _, _, err := a.service.CreateTimeline(ctx, "tl-sx", timelineReq("sx", srcX, 480)); err != nil {
		fatal(err)
	}
	if _, _, err := b.service.CreateTimeline(ctx, "tl-sy", timelineReq("sy", srcY, 480)); err != nil {
		fatal(err)
	}
	renderID := "rnd-seq"
	jobA, cA, eA := a.service.CreateRender(ctx, renderID, service.RenderWriteRequest{TimelineID: "tl-sx"})
	jobBRetry, cB1, eB1 := b.service.CreateRender(ctx, renderID, service.RenderWriteRequest{TimelineID: "tl-sx"})
	jobBConflict, cB2, eB2 := b.service.CreateRender(ctx, renderID, service.RenderWriteRequest{TimelineID: "tl-sy"})
	_ = trace.writeFile("interleaving-trace.txt")
	result := ScenarioResult{
		Scenario: "R4-sequential-render-retry-conflict",
		Purpose:  "non-overlapping render calls: idempotent retry versus explicit render_conflict across processes",
		Outcomes: map[string]any{
			"A-first":             jobOutcome(jobA, cA, eA),
			"B-retry-same-tl":     jobOutcome(jobBRetry, cB1, eB1),
			"B-conflict-other-tl": jobOutcome(jobBConflict, cB2, eB2),
		},
		Files: snapshotDisk(dataDir),
	}
	retryOK := eB1 == nil && !cB1 && jobBRetry.ID == renderID
	conflictOK := eB2 != nil && errCode(eB2)["code"] == "render_conflict"
	if cA && retryOK && conflictOK {
		result.Verdict = "EXPECTED_SEMANTICS_PRESERVED: identical retry returns the existing job with 200; a different timeline yields a clean 409 render_conflict"
	} else {
		result.Verdict = "UNEXPECTED: inspect trace"
	}
	return result
}

func scenarioRenderDistinctControl(ctx context.Context, dataDir, outDir string, trace *Trace) ScenarioResult {
	srcX, srcY, _, _ := setupTwoSources(ctx, dataDir, trace, NewBarrier(trace))
	a := newStack("instance-A", dataDir, trace, NewBarrier(trace), nil)
	b := newStack("instance-B", dataDir, trace, NewBarrier(trace), nil)
	if _, _, err := a.service.CreateTimeline(ctx, "tl-cx", timelineReq("cx", srcX, 480)); err != nil {
		fatal(err)
	}
	if _, _, err := b.service.CreateTimeline(ctx, "tl-cy", timelineReq("cy", srcY, 480)); err != nil {
		fatal(err)
	}
	type pair struct {
		who     string
		job     domain.RenderJob
		created bool
		err     error
	}
	ch := make(chan pair, 2)
	go func() {
		job, created, err := a.service.CreateRender(ctx, "rnd-alpha", service.RenderWriteRequest{TimelineID: "tl-cx"})
		ch <- pair{"A", job, created, err}
	}()
	go func() {
		job, created, err := b.service.CreateRender(ctx, "rnd-beta", service.RenderWriteRequest{TimelineID: "tl-cy"})
		ch <- pair{"B", job, created, err}
	}()
	outcomes := map[string]any{}
	for i := 0; i < 2; i++ {
		r := <-ch
		outcomes["call-"+r.who] = jobOutcome(r.job, r.created, r.err)
	}
	_ = trace.writeFile("interleaving-trace.txt")
	result := ScenarioResult{
		Scenario: "R3-distinct-render-ids-control",
		Purpose:  "parallel renders with distinct ids: the legal control case",
		Outcomes: map[string]any{"responses": outcomes},
		Files:    snapshotDisk(dataDir),
	}
	ok := true
	for _, raw := range outcomes {
		if raw.(callOutcome).Status != "201_created" {
			ok = false
		}
	}
	if ok {
		result.Verdict = "CONTROL_OK: both render jobs completed independently with their own receipts"
	} else {
		result.Verdict = "CONTROL_FAILED"
	}
	return result
}

// waitForTrace spins until the ordered event log contains a fragment.
func waitForTrace(trace *Trace, fragment string) {
	deadline := deadlineAfter(20_000)
	for {
		trace.mu.Lock()
		found := false
		for _, event := range trace.events {
			if strings.Contains(event, fragment) {
				found = true
				break
			}
		}
		trace.mu.Unlock()
		if found {
			return
		}
		if deadline() {
			fatal(fmt.Errorf("timeout waiting for trace fragment %q", fragment))
		}
		sleep5ms()
	}
}

// waitForTraceCount waits until fragment appears at least count times.
func waitForTraceCount(trace *Trace, fragment string, count int) {
	deadline := deadlineAfter(20_000)
	for {
		trace.mu.Lock()
		seen := 0
		for _, event := range trace.events {
			if strings.Contains(event, fragment) {
				seen++
			}
		}
		trace.mu.Unlock()
		if seen >= count {
			return
		}
		if deadline() {
			fatal(fmt.Errorf("timeout waiting for %d occurrences of %q", count, fragment))
		}
		sleep5ms()
	}
}
