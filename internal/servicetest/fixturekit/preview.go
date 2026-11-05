package fixturekit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// PreviewServiceID is an engine-issued scalar ServiceID whose protobuf holds
// an opaque engine result and rootType Service!.
const PreviewServiceID = "EhEIsdUfEgsKB1NlcnZpY2UYAQ=="

// PreviewRecipeServiceID uses the recipe DAG encoding: rootDigest and
// callsByDigest select an asService Call of type Service!.
const PreviewRecipeServiceID = "ChV4eGgzOjAxMjM0NTY3ODlhYmNkZWYSSAoVeHhoMzowMTIzNDU2Nzg5YWJjZGVmEi8SCwoHU2VydmljZRgBGglhc1NlcnZpY2VKFXh4aDM6MDEyMzQ1Njc4OWFiY2RlZg=="

// PreviewCLIs adds an offline Service and Chromium capture to the engine fakes.
// Failure contains stderr for a failed capture; Block holds a capture until its
// context is cancelled. Files record the argv, exported source and environment.
type PreviewCLIs struct {
	EngineCLIs
	Call, Capture, Source, Environment, Runs, Failure, Block, ServiceID string
}

func InstallPreviewCLIs(t *testing.T, png []byte) PreviewCLIs {
	t.Helper()
	engine := InstallEngineCLIs(t)
	f := PreviewCLIs{EngineCLIs: engine}
	f.Call = filepath.Join(engine.Directory, "preview-call")
	f.Capture = filepath.Join(engine.Directory, "preview-capture")
	f.Source = filepath.Join(engine.Directory, "preview-source")
	f.Environment = filepath.Join(engine.Directory, "preview-environment")
	f.Runs = filepath.Join(engine.Directory, "preview-runs")
	f.Failure = filepath.Join(engine.Directory, "preview-failure")
	f.ServiceID = filepath.Join(engine.Directory, "preview-service-id")
	f.Block = filepath.Join(engine.Directory, "preview-block")
	image := filepath.Join(engine.Directory, "image.png")
	Must(t, os.WriteFile(image, png, 0600))
	fallback := filepath.Join(engine.Directory, "dagger-engine")
	Must(t, os.Rename(filepath.Join(engine.Directory, "dagger"), fallback))
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
 call)
  printf '%%s\000' "$@" > %q
  cat preview-source.txt > %q
  env > %q
  if [ -f %q ]; then cat %q; else printf '%%s\n' %q; fi
  ;;
 core)
  if [ "${2-}" != container ]; then exec %q "$@"; fi
  printf '%%s\000' "$@" > %q
  printf 'capture\n' >> %q
  env >> %q
  if [ -f %q ]; then cat %q >&2; exec sleep 120; fi
  if [ -f %q ]; then cat %q >&2; exit 7; fi
  for arg do output=$arg; done
  cp %q "$output"
  ;;
 *) exec %q "$@" ;;
esac
`, f.Call, f.Source, f.Environment, f.ServiceID, f.ServiceID, PreviewServiceID, fallback, f.Capture, f.Runs, f.Environment, f.Block, f.Block, f.Failure, f.Failure, image, fallback)
	Must(t, os.WriteFile(filepath.Join(engine.Directory, "dagger"), []byte(script), 0700))
	return f
}
