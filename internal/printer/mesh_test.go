package printer

import (
	"errors"
	"testing"
)

func TestParseMeshReportNoData(t *testing.T) {
	active, _, points, ok := ParseMeshReport([]string{"Mesh Bed Leveling has no data.", "ok"})
	if !ok || active || points != nil {
		t.Fatalf("active=%v points=%v ok=%v", active, points, ok)
	}
}

func TestParseMeshReportGrid(t *testing.T) {
	lines := []string{
		"Mesh Bed Leveling ON",
		"5x5 mesh. Z offset: -0.12500",
		"",
		"Measured points:",
		"  0    1    2    3    4",
		" 0 +0.00000 +0.01000 +0.02000 +0.01000 +0.00000",
		" 1 +0.00500 +0.01500 +0.02500 +0.01500 +0.00500",
		" 2 +0.01000 +0.02000 +0.03000 +0.02000 +0.01000",
		" 3 +0.00500 +0.01500 +0.02500 +0.01500 +0.00500",
		" 4 +0.00000 +0.01000 +0.02000 +0.01000 +0.00000",
		"ok",
	}
	active, zOffset, points, ok := ParseMeshReport(lines)
	if !ok || !active {
		t.Fatalf("active=%v ok=%v", active, ok)
	}
	if zOffset != -0.125 {
		t.Fatalf("zOffset = %v", zOffset)
	}
	if len(points) != 5 || len(points[0]) != 5 {
		t.Fatalf("points shape = %+v", points)
	}
	if points[2][2] != 0.03 {
		t.Fatalf("points[2][2] = %v, want 0.03 (row=y, col=x)", points[2][2])
	}
	if points[0][0] != 0 || points[4][4] != 0 {
		t.Fatalf("corner points = %v / %v", points[0][0], points[4][4])
	}
}

func TestParseMBLPointLine(t *testing.T) {
	p, tot, ok := ParseMBLPointLine("MBL G29 point 13 of 25")
	if !ok || p != 13 || tot != 25 {
		t.Fatalf("p=%d tot=%d ok=%v", p, tot, ok)
	}
	if _, _, ok := ParseMBLPointLine("ok"); ok {
		t.Fatal("expected no match")
	}
	// The finishing call reports a negative index (see G29.cpp:260); it
	// must still parse, ParseMeshDone is what signals "done", not this.
	p, tot, ok = ParseMBLPointLine("MBL G29 point -1 of 25")
	if !ok || p != -1 || tot != 25 {
		t.Fatalf("p=%d tot=%d ok=%v", p, tot, ok)
	}
}

func TestParseMeshDone(t *testing.T) {
	if !ParseMeshDone("Mesh probing done.") {
		t.Fatal("expected match")
	}
	if ParseMeshDone("MBL G29 point 1 of 25") {
		t.Fatal("expected no match")
	}
}

func TestParsePositionZ(t *testing.T) {
	z, ok := ParsePositionZ("X:0.00 Y:0.00 Z:0.20 E:0.00 Count X:0 Y:0 Z:80 E:0")
	if !ok || z != 0.2 {
		t.Fatalf("z=%v ok=%v", z, ok)
	}
	if _, ok := ParsePositionZ("ok"); ok {
		t.Fatal("expected no match")
	}
}

// TestMeshReportNoData exercises Manager.MeshReport end to end (G29 S0)
// before any probe has run.
func TestMeshReportNoData(t *testing.T) {
	mgr, _ := jtRig(t, nil)
	mesh, err := mgr.MeshReport()
	if err != nil {
		t.Fatal(err)
	}
	if mesh.Active || mesh.Points != nil {
		t.Fatalf("mesh = %+v", mesh)
	}
}

// TestMeshLevelFullFlow drives Start then Next 25 times (the real G29
// S1/S2 rhythm: one point becomes active per call, matching Marlin's own
// "MBL G29 point N of 25" numbering), checks the snapshot along the way,
// and confirms the mesh is active and reportable once done.
func TestMeshLevelFullFlow(t *testing.T) {
	mgr, _ := jtRig(t, nil)
	lv, err := mgr.MeshLevelStart()
	if err != nil {
		t.Fatal(err)
	}
	if lv.Point != 1 || lv.Total != 25 {
		t.Fatalf("start: %+v", lv)
	}
	if snap := mgr.Snapshot(); snap.Leveling == nil || snap.Leveling.Point != 1 {
		t.Fatalf("snapshot leveling = %+v", snap.Leveling)
	}
	var done bool
	for i := 2; i <= 25; i++ {
		lv, done, err = mgr.MeshLevelNext()
		if err != nil {
			t.Fatalf("next point %d: %v", i, err)
		}
		if done {
			t.Fatalf("next point %d reported done early", i)
		}
		if lv.Point != i {
			t.Fatalf("next point %d: got point %d", i, lv.Point)
		}
	}
	lv, done, err = mgr.MeshLevelNext()
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatalf("expected done after point 25, got %+v", lv)
	}
	if snap := mgr.Snapshot(); snap.Leveling != nil {
		t.Fatalf("snapshot leveling should be cleared once done, got %+v", snap.Leveling)
	}
	mesh, err := mgr.MeshReport()
	if err != nil {
		t.Fatal(err)
	}
	if !mesh.Active || len(mesh.Points) != 5 {
		t.Fatalf("mesh after finish = %+v", mesh)
	}
}

func TestMeshLevelAbortClearsStateAndLiftsZ(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if _, err := mgr.MeshLevelStart(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.MeshLevelAbort(); err != nil {
		t.Fatal(err)
	}
	if snap := mgr.Snapshot(); snap.Leveling != nil {
		t.Fatalf("leveling should be cleared after abort, got %+v", snap.Leveling)
	}
	jtWait(t, "abort sequence", func() bool { return f.count("G1 Z10 F600") == 1 && f.count("M211 S1") == 1 })
}

func TestMeshLevelFinishSaveAndDiscard(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.MeshLevelFinish(true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "M420/M500", func() bool { return f.count("M420 S1") == 1 && f.count("M500") == 1 })

	if err := mgr.MeshLevelFinish(false); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "G29 S5", func() bool { return f.count("G29 S5") == 1 })
}

func TestMeshZAdjustClampsAndRequiresLeveling(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if _, err := mgr.MeshZAdjust(0.5); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("without a leveling wizard: %v", err)
	}
	if _, err := mgr.MeshLevelStart(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.MeshZAdjust(0); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("zero delta: %v", err)
	}
	// 2mm requested, clamped down to zAdjustMax (1mm) added to the Z G29
	// S1 last reported (0.21, from the fake's step 1 reply).
	z, err := mgr.MeshZAdjust(2)
	if err != nil {
		t.Fatal(err)
	}
	if z <= 0.21 || z > 1.22 {
		t.Fatalf("z = %v, want clamped to +1mm over the start Z", z)
	}
	jtWait(t, "G1 Z clamped", func() bool { return f.count("M114") >= 1 })
	// A tiny nudge is clamped up to zAdjustMin (0.025mm), not rejected.
	if _, err := mgr.MeshZAdjust(0.001); err != nil {
		t.Fatal(err)
	}
}

func TestMeshSetPointValidatesBounds(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.MeshSetPoint(-1, 0, 0, false); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("x=-1: %v", err)
	}
	if err := mgr.MeshSetPoint(0, 5, 0, false); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("y=5: %v", err)
	}
	if err := mgr.MeshSetPoint(2, 3, 0.04, true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "G29 S3", func() bool { return f.count("G29 S3 I2 J3 Z0.04") == 1 && f.count("M500") == 1 })
}

func TestMeshZOffsetClampsAndSaves(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.MeshZOffset(3, true); err != nil { // clamped to +2 (zOffsetRange)
		t.Fatal(err)
	}
	jtWait(t, "G29 S4 clamped + M500", func() bool { return f.count("G29 S4 Z2") == 1 && f.count("M500") == 1 })
	if err := mgr.MeshZOffset(-0.075, false); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "G29 S4 no save", func() bool { return f.count("G29 S4 Z-0.075") == 1 })
	if f.count("M500") != 1 {
		t.Fatal("M500 sent without save=true")
	}
}

func TestBedCornerSequenceAndValidation(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.BedCorner(Corner("nope")); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad corner: %v", err)
	}
	if err := mgr.BedCorner(CornerFrontLeft); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "corner sequence", func() bool {
		return f.count("G28") == 1 && f.count("G1 Z5 F600") == 1 && f.count("G1 X30 Y30 F3000") == 1 && f.count("G1 Z0 F300") == 1
	})
}

// TestMeshBlockedDuringJob covers R-05-style motion safety for every mesh
// and bed route: none of them may reach the printer while a job is
// active.
func TestMeshBlockedDuringJob(t *testing.T) {
	mgr, _ := jtRig(t, nil)
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.MeshReport(); !errors.Is(err, ErrJobActive) {
		t.Fatalf("MeshReport during job = %v", err)
	}
	if _, err := mgr.MeshLevelStart(); !errors.Is(err, ErrJobActive) {
		t.Fatalf("MeshLevelStart during job = %v", err)
	}
	if err := mgr.MeshLevelAbort(); !errors.Is(err, ErrJobActive) {
		t.Fatalf("MeshLevelAbort during job = %v", err)
	}
	if err := mgr.MeshSetPoint(0, 0, 0, false); !errors.Is(err, ErrJobActive) {
		t.Fatalf("MeshSetPoint during job = %v", err)
	}
	if err := mgr.MeshZOffset(0, false); !errors.Is(err, ErrJobActive) {
		t.Fatalf("MeshZOffset during job = %v", err)
	}
	if err := mgr.BedCorner(CornerCenter); !errors.Is(err, ErrJobActive) {
		t.Fatalf("BedCorner during job = %v", err)
	}
}
