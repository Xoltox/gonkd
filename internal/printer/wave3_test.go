package printer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// wave3Gcode writes a small file with a layer table (";LAYER_CHANGE"/
// ";Z:"), an "M73 P.. R.." checkpoint and a comment, so the upload scan
// (metaParser) has something to record and the card-space byte count
// (comment-stripped) differs from the raw file size.
func wave3Gcode(t *testing.T) string {
	t.Helper()
	content := "; a leading comment stripped from the card\n" +
		";LAYER_CHANGE\n" +
		";Z:0.2\n" +
		"G28\n" +
		"G1 X1 Y1 Z0.2\n" +
		"M73 P50 R10\n" +
		";LAYER_CHANGE\n" +
		";Z:0.4\n" +
		"G1 X2 Y2 Z0.4\n"
	path := filepath.Join(t.TempDir(), "wave3.gcode")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- 1: file list upsert after upload, no M20 ---

func TestUploadUpsertsListingWithoutM20(t *testing.T) {
	mgr, f := jtRig(t, nil)
	path := wave3Gcode(t)
	// UploadToSD itself sends one M20 L up front to pick a safe short name;
	// what must NOT happen is another one after the upload finishes (the
	// old behavior on the startPrint=false path). Wait for Start's own
	// init-time M20 L to have gone out before taking the baseline.
	jtWait(t, "initial M20", func() bool { return f.count("M20") >= 1 })
	before := f.count("M20")
	if err := mgr.Upload(path, "wave3.gcode", ModeSDUpload, false); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	jtWait(t, "job ends idle", func() bool { return mgr.Snapshot().State == StateIdle })

	if n := f.count("M20") - before; n != 1 {
		t.Fatalf("M20 sent %d times during Upload, want exactly 1 (the pre-upload listing, no post-upload refresh)", n)
	}
	files := mgr.SDFiles()
	if len(files) != 1 {
		t.Fatalf("SDFiles = %v, want exactly one entry", files)
	}
	if files[0].Long != "wave3.gcode" {
		t.Fatalf("Long = %q, want wave3.gcode", files[0].Long)
	}
	if files[0].Bytes <= 0 {
		t.Fatalf("Bytes = %d, want > 0", files[0].Bytes)
	}
}

// --- 2/3: card-space offsets and layer/M73 tables, trimmed from listing ---

func TestUploadRecordsLayersAndM73InCardSpace(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := jtRig(t, nil)
	mgr.SetMetaStore(NewMetaStore(filepath.Join(dir, "meta")))

	path := wave3Gcode(t)
	if err := mgr.Upload(path, "wave3.gcode", ModeSDUpload, false); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	jtWait(t, "job ends idle", func() bool { return mgr.Snapshot().State == StateIdle })

	files := mgr.SDFiles()
	if len(files) != 1 {
		t.Fatalf("SDFiles = %v, want one entry", files)
	}
	short := files[0].Short
	cardBytes := files[0].Bytes

	// The /gonkd/files listing must not carry the tables.
	if files[0].Meta == nil {
		t.Fatal("Meta = nil, want parsed metadata")
	}
	if files[0].Meta.Layers != nil || files[0].Meta.M73 != nil {
		t.Fatalf("listing Meta carries Layers/M73: %+v", files[0].Meta)
	}
	if files[0].Meta.LayerCount != 2 {
		t.Fatalf("LayerCount = %d, want 2", files[0].Meta.LayerCount)
	}

	// The full stored metadata (server-side only) does carry them, with
	// offsets in card space: the leading comment contributes 0 bytes, so
	// the first layer starts at offset 0, strictly before the second.
	full := mgr.meta.Get(short)
	if full == nil {
		t.Fatal("stored metadata missing")
	}
	if len(full.Layers) != 2 {
		t.Fatalf("Layers = %+v, want 2 entries", full.Layers)
	}
	if full.Layers[0].O != 0 {
		t.Fatalf("Layers[0].O = %d, want 0 (comments contribute no card bytes)", full.Layers[0].O)
	}
	if full.Layers[1].O <= full.Layers[0].O {
		t.Fatalf("Layers[1].O = %d, want > Layers[0].O = %d", full.Layers[1].O, full.Layers[0].O)
	}
	if full.Layers[1].O >= cardBytes {
		t.Fatalf("Layers[1].O = %d, want < cardBytes = %d", full.Layers[1].O, cardBytes)
	}
	if len(full.M73) != 1 || full.M73[0].P != 50 || full.M73[0].R != 600 {
		t.Fatalf("M73 = %+v, want one {P:50 R:600}", full.M73)
	}
}

// --- 4: Job.Layer/LayerTotal/Z/ETASec/EtaSource ---

func TestApplyPrintProgressM73Best(t *testing.T) {
	meta := &SDMeta{
		LayerCount: 3,
		Layers:     []LayerPoint{{O: 0, Z: 0.2}, {O: 500, Z: 0.4}, {O: 1000, Z: 0.6}},
		M73:        []M73Point{{O: 0, P: 0, R: 100}, {O: 1000, P: 100, R: 0}},
	}
	job := &Job{SentBytes: 500, TotalBytes: 2000, firstByteAt: time.Now().Add(-10 * time.Second)}
	applyPrintProgress(job, meta)
	if job.EtaSource != "m73" {
		t.Fatalf("EtaSource = %q, want m73", job.EtaSource)
	}
	if job.ETASec != 50 {
		t.Fatalf("ETASec = %v, want 50 (halfway between 100 and 0)", job.ETASec)
	}
	if job.Layer != 2 || job.Z != 0.4 {
		t.Fatalf("Layer/Z = %d/%v, want 2/0.4", job.Layer, job.Z)
	}
	if job.LayerTotal != 3 {
		t.Fatalf("LayerTotal = %d, want 3", job.LayerTotal)
	}
}

func TestApplyPrintProgressSlicerFallback(t *testing.T) {
	meta := &SDMeta{EstSec: 1000}
	job := &Job{SentBytes: 500, TotalBytes: 1000, firstByteAt: time.Now().Add(-1000 * time.Second)}
	applyPrintProgress(job, meta)
	if job.EtaSource != "slicer" {
		t.Fatalf("EtaSource = %q, want slicer", job.EtaSource)
	}
	// Actual pace (1000s elapsed at 50%) is twice the slicer's own
	// (500s at 50%), so the adjusted estimate doubles to 2000s and ETA is
	// what's left of that past the 1000s already elapsed.
	if d := job.ETASec - 1000; d < -1 || d > 1 {
		t.Fatalf("ETASec = %v, want ~1000", job.ETASec)
	}
}

func TestApplyPrintProgressLiveFallback(t *testing.T) {
	job := &Job{SentBytes: 250, TotalBytes: 1000, firstByteAt: time.Now().Add(-100 * time.Second)}
	applyPrintProgress(job, nil)
	if job.EtaSource != "live" {
		t.Fatalf("EtaSource = %q, want live", job.EtaSource)
	}
	if d := job.ETASec - 300; d < -1 || d > 1 {
		t.Fatalf("ETASec = %v, want ~300 (25%% done in 100s => 400s total)", job.ETASec)
	}
}

func TestApplyPrintProgressNoDataBeforeFirstByte(t *testing.T) {
	job := &Job{SentBytes: 0, TotalBytes: 1000}
	applyPrintProgress(job, &SDMeta{EstSec: 500})
	if job.EtaSource != "" || job.ETASec != 0 {
		t.Fatalf("job = %+v, want untouched before the first SD byte", job)
	}
}

// --- 5: position (M114) ---

func TestRequestPositionRateLimitedAndParsed(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.RequestPosition(); err != nil {
		t.Fatalf("RequestPosition: %v", err)
	}
	jtWait(t, "position parsed", func() bool {
		s := mgr.Snapshot()
		return s.Position != nil
	})
	if n := f.count("M114"); n != 1 {
		t.Fatalf("M114 count = %d, want 1", n)
	}
	// A second call within the rate-limit window is a silent no-op.
	if err := mgr.RequestPosition(); err != nil {
		t.Fatalf("RequestPosition (rate-limited): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.count("M114"); n != 1 {
		t.Fatalf("M114 count after a second immediate call = %d, want still 1", n)
	}
}

func TestHomeTriggersPositionRefresh(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.SendSequence([]string{"G28"}); err != nil {
		t.Fatalf("SendSequence: %v", err)
	}
	jtWait(t, "M114 after home", func() bool { return f.count("M114") >= 1 })
}

// --- 6: mesh cache ---

func TestMeshCachedWhileJobRuns(t *testing.T) {
	mgr, _ := jtRig(t, func(f *jtFake) { f.saveDelay = 10 * time.Millisecond })
	if _, err := mgr.MeshReport(); err != nil {
		t.Fatalf("MeshReport: %v", err)
	}
	path := jtGcode(t, 150)
	if err := mgr.Upload(path, "busy.gcode", ModeSDUpload, false); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	jtWait(t, "uploading", func() bool { return mgr.Snapshot().State == StateUploading })

	mesh, err := mgr.MeshReportOrCached()
	if err != nil {
		t.Fatalf("MeshReportOrCached: %v", err)
	}
	if !mesh.Cached {
		t.Fatalf("mesh = %+v, want Cached true while a job runs", mesh)
	}
	jtWait(t, "upload finishes", func() bool { return mgr.Snapshot().State == StateIdle })
}

func TestMeshNoCacheReturnsErrNoMesh(t *testing.T) {
	mgr, _ := jtRig(t, func(f *jtFake) { f.saveDelay = 10 * time.Millisecond })
	path := jtGcode(t, 150)
	if err := mgr.Upload(path, "busy.gcode", ModeSDUpload, false); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	jtWait(t, "uploading", func() bool { return mgr.Snapshot().State == StateUploading })

	if _, err := mgr.MeshReportOrCached(); err != ErrNoMesh {
		t.Fatalf("MeshReportOrCached = %v, want ErrNoMesh", err)
	}
	jtWait(t, "upload finishes", func() bool { return mgr.Snapshot().State == StateIdle })
}
