package errorreport

import "testing"

func fingerprintOf(t *testing.T, mutate func(*Event)) string {
	t.Helper()
	ev := Event{
		Source:   SourceOperation,
		Template: "draft.seal",
		Status:   500,
		Code:     "seal_failed",
		Message:  "sealing w_eac398a71ab8c125588dd29e5e0c7473 failed at 2026-11-20T10:00:00Z after 1.5s: exit 3 (pid 4012) commit 0123456789abcdef0123456789abcdef01234567 id 8f14e45f-ceea-467a-9575-e6f3d0e0e7d1",
	}
	if mutate != nil {
		mutate(&ev)
	}
	rep, err := testScrubber().Report(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fingerprint) != 16 {
		t.Fatalf("fingerprint %q", rep.Fingerprint)
	}
	return rep.Fingerprint
}

func TestFingerprintIgnoresIDsHashesNumbersAndTimestamps(t *testing.T) {
	t.Parallel()
	base := fingerprintOf(t, nil)
	same := fingerprintOf(t, func(e *Event) {
		e.Message = "sealing w_00000000000000000000000000000000 failed at 2027-01-02 03:04:05 after 2m31s: exit 128 (pid 17) commit ffffffffffffffffffffffffffffffffffffffff id 00000000-0000-0000-0000-000000000000"
	})
	if base != same {
		t.Fatalf("fingerprints differ for messages that differ only in IDs, hashes, numbers and timestamps: %s %s", base, same)
	}
}

func TestFingerprintDiffers(t *testing.T) {
	t.Parallel()
	base := fingerprintOf(t, nil)
	for name, mutate := range map[string]func(*Event){
		"source":   func(e *Event) { e.Source = SourceAPI },
		"template": func(e *Event) { e.Template = "draft.land" },
		"status":   func(e *Event) { e.Status = 502 },
		"code":     func(e *Event) { e.Code = "other_code" },
		"message":  func(e *Event) { e.Message = "landing refused for w_eac398a71ab8c125588dd29e5e0c7473" },
	} {
		if got := fingerprintOf(t, mutate); got == base {
			t.Errorf("fingerprint did not change with the %s", name)
		}
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	got := Normalize("took 1.5s, 120ms and 2m30s at 10:11:12 on 2026-11-20 for p_ab74f84ae6b1d23cdeed8d6806da664f, try 7 of 10  abcdef1234567")
	want := "took <n>, <n> and <n> at <time> on <time> for <id>, try <n> of <n> <hash>"
	if got != want {
		t.Fatalf("Normalize = %q, want %q", got, want)
	}
}
