package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arrase/gcp-arch/internal/render"
	"github.com/arrase/gcp-arch/internal/synth"
)

const (
	stabilityRuns  = 3
	liveCmdTimeout = 5 * time.Minute
)

func requireLiveEnv(t *testing.T) (bin, vertexProject, vertexLocation, vertexModel string) {
	t.Helper()
	if os.Getenv("GCP_ARCH_E2E") != "1" {
		t.Skip("skipping live Vertex AI + Chrome E2E test (set GCP_ARCH_E2E=1 to enable)")
	}
	vertexProject = strings.TrimSpace(os.Getenv("GCP_ARCH_VERTEX_PROJECT"))
	if vertexProject == "" {
		t.Fatal("GCP_ARCH_VERTEX_PROJECT must be set when GCP_ARCH_E2E=1")
	}
	vertexLocation = strings.TrimSpace(os.Getenv("GCP_ARCH_VERTEX_LOCATION"))
	if vertexLocation == "" {
		vertexLocation = synth.DefaultVertexLocation
	}
	vertexModel = strings.TrimSpace(os.Getenv("GCP_ARCH_VERTEX_MODEL"))
	if vertexModel == "" {
		vertexModel = synth.DefaultVertexModel
	}

	root := repoRoot(t)
	bin = filepath.Join(t.TempDir(), "gcp-arch")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gcp-arch")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ./cmd/gcp-arch: %v\n%s", err, string(out))
	}
	return bin, vertexProject, vertexLocation, vertexModel
}

func runLiveCase(t *testing.T, bin, root, factsPath, caseDir, project, location, model string) {
	t.Helper()
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archPath := filepath.Join(caseDir, "architecture.yaml")

	ctxSynth, cancelSynth := context.WithTimeout(context.Background(), liveCmdTimeout)
	defer cancelSynth()
	synthCmd := exec.CommandContext(ctxSynth,
		bin, "synthesize", factsPath,
		"--vertex-project", project,
		"--vertex-location", location,
		"--model", model,
		"-o", archPath,
	)
	synthCmd.Dir = root
	if out, err := synthCmd.CombinedOutput(); err != nil {
		t.Fatalf("synthesize %s failed: %v\n%s", filepath.Base(factsPath), err, string(out))
	}

	ctxRender, cancelRender := context.WithTimeout(context.Background(), liveCmdTimeout)
	defer cancelRender()
	renderCmd := exec.CommandContext(ctxRender,
		bin, "render", archPath,
		"--format", "png,html",
		"--facts", factsPath,
		"--geometry-report",
		"-o", caseDir,
	)
	renderCmd.Dir = root
	if out, err := renderCmd.CombinedOutput(); err != nil {
		t.Fatalf("render %s failed: %v\n%s", filepath.Base(factsPath), err, string(out))
	}
}

func TestCatalogLiveE2E(t *testing.T) {
	bin, project, location, model := requireLiveEnv(t)
	root := repoRoot(t)
	catalogDir := filepath.Join(root, "testdata", "catalog")

	fixtures, err := filepath.Glob(filepath.Join(catalogDir, "*.facts.json"))
	if err != nil {
		t.Fatalf("glob catalog fixtures: %v", err)
	}
	if len(fixtures) < 10 {
		t.Fatalf("expected >= 10 catalog fixtures in %s, got %d", catalogDir, len(fixtures))
	}

	manifestBytes, err := os.ReadFile(filepath.Join(catalogDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read catalog manifest.json: %v", err)
	}
	var catManifest catalogManifest
	if err := json.Unmarshal(manifestBytes, &catManifest); err != nil {
		t.Fatalf("decode catalog manifest.json: %v", err)
	}
	byName := make(map[string]catalogManifestCase, len(catManifest.Fixtures))
	for _, mc := range catManifest.Fixtures {
		byName[mc.Name] = mc
	}

	outDir := filepath.Join(t.TempDir(), "catalog-out")
	for _, fpath := range fixtures {
		caseName := strings.TrimSuffix(filepath.Base(fpath), ".facts.json")
		caseDir := filepath.Join(outDir, caseName)
		t.Run(caseName, func(t *testing.T) {
			runLiveCase(t, bin, root, fpath, caseDir, project, location, model)
			verifyLiveCatalogCase(t, caseName, caseDir, byName)
		})
	}
}

func verifyLiveCatalogCase(t *testing.T, caseName, caseDir string, byName map[string]catalogManifestCase) {
	t.Helper()
	if err := verifyGeometryCase(caseDir); err != nil {
		t.Fatalf("verifyGeometryCase(%s) failed: %v", caseDir, err)
	}
	mc, ok := byName[caseName]
	if !ok {
		t.Fatalf("case %q missing from manifest.json", caseName)
	}
	rawReport, err := os.ReadFile(filepath.Join(caseDir, "geometry_report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report render.GeometryReport
	if err := json.Unmarshal(rawReport, &report); err != nil {
		t.Fatal(err)
	}
	if report.Conservation == nil {
		t.Fatal("geometry_report.json missing conservation block")
	}
	if report.Conservation.FactsPrimaryCount != len(mc.ExpectedPrimaryURIs) {
		t.Fatalf("facts_primary_count=%d != manifest expected_primary_uris=%d", report.Conservation.FactsPrimaryCount, len(mc.ExpectedPrimaryURIs))
	}
	if report.Conservation.RepresentedPrimaryCount != len(mc.ExpectedPrimaryURIs) {
		t.Fatalf("represented_primary_count=%d != manifest expected_primary_uris=%d", report.Conservation.RepresentedPrimaryCount, len(mc.ExpectedPrimaryURIs))
	}
	if report.Conservation.FactsFunctionalEdgeCount != len(mc.ExpectedFunctionalEdgeIDs) {
		t.Fatalf("facts_functional_edge_count=%d != manifest expected_functional_edge_ids=%d", report.Conservation.FactsFunctionalEdgeCount, len(mc.ExpectedFunctionalEdgeIDs))
	}
}

func TestCatalogStabilityLiveE2E(t *testing.T) {
	bin, project, location, model := requireLiveEnv(t)
	root := repoRoot(t)

	const targetCase = "cat3_shared_vpc_hybrid_armor"
	srcFixture := filepath.Join(root, "testdata", "catalog", targetCase+".facts.json")

	archPaths := make([]string, 0, stabilityRuns)
	for i := range stabilityRuns {
		caseDir := filepath.Join(t.TempDir(), targetCase)
		runLiveCase(t, bin, root, srcFixture, caseDir, project, location, model)
		if err := verifyGeometryCase(caseDir); err != nil {
			t.Fatalf("stability run %d verifyGeometryCase failed: %v", i, err)
		}
		archPaths = append(archPaths, filepath.Join(caseDir, "architecture.yaml"))
	}

	if err := checkStability(archPaths); err != nil {
		t.Fatalf("checkStability failed across %d runs: %v", stabilityRuns, err)
	}
}
