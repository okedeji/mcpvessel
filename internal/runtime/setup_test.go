package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/progress"
)

// The markers come from real limactl create/start runs on macOS. This pins
// the parsing so a Lima upgrade that quietly renames a log line fails CI
// rather than silently regressing the UX.
func TestSetupTap_DetectsLimaPhases(t *testing.T) {
	var buf bytes.Buffer
	ui := progress.NewSetupPlain(&buf, "", "", SetupPhases)

	tap := newSetupTap(ui)
	limaOutput := strings.Join([]string{
		`INFO[0000] Creating an instance "agentcage"`,
		`INFO[0005] Pulling: https://cloud-images.ubuntu.com/.../ubuntu.img`,
		`INFO[0020] Downloading the image: 142 MB / 600 MB`,
		`INFO[0050] Created an instance "agentcage"`,
		`INFO[0051] Starting the instance "agentcage" with VM driver "vz"`,
		`INFO[0060] [hostagent] Waiting for the guest agent to be running`,
		`INFO[0062] [hostagent] Waiting for the final requirement 1 of 1: "boot scripts must have finished"`,
		`INFO[0090] READY. Run ` + "`limactl shell agentcage`" + ` to open the shell.`,
		"",
	}, "\n")

	if _, err := tap.Write([]byte(limaOutput)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ui.Done()

	out := buf.String()
	// Order matters: Preparing happens before Booting.
	prepIdx := strings.Index(out, "-> "+SetupPhasePreparing)
	bootIdx := strings.Index(out, "-> "+SetupPhaseBooting)
	if prepIdx < 0 {
		t.Errorf("expected %q phase, got:\n%s", SetupPhasePreparing, out)
	}
	if bootIdx < 0 {
		t.Errorf("expected %q phase, got:\n%s", SetupPhaseBooting, out)
	}
	if prepIdx > 0 && bootIdx > 0 && prepIdx > bootIdx {
		t.Errorf("phase order wrong: preparing should precede booting, got:\n%s", out)
	}
}

func TestSetupTap_BuffersAcrossWrites(t *testing.T) {
	// Provisioner output arrives in chunks that rarely align to line
	// boundaries; the tap must buffer across writes or lose markers.
	var buf bytes.Buffer
	ui := progress.NewSetupPlain(&buf, "", "", SetupPhases)
	tap := newSetupTap(ui)

	full := `INFO[0000] Creating an instance "agentcage"` + "\n"
	mid := len(full) / 2
	if _, err := tap.Write([]byte(full[:mid])); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := tap.Write([]byte(full[mid:])); err != nil {
		t.Fatalf("second write: %v", err)
	}
	ui.Done()

	if !strings.Contains(buf.String(), "-> "+SetupPhasePreparing) {
		t.Errorf("split write across \\n boundary lost the phase marker:\n%s", buf.String())
	}
}

func TestSetupTap_UnknownLinesAreDiscarded(t *testing.T) {
	var buf bytes.Buffer
	ui := progress.NewSetupPlain(&buf, "", "", SetupPhases)
	tap := newSetupTap(ui)
	noisy := strings.Repeat("noise that is not a phase marker\n", 10)
	if _, err := tap.Write([]byte(noisy)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ui.Done()
	// With no transitions, at most the final "Setup complete" line appears
	// and none of the raw input leaks.
	if strings.Contains(buf.String(), "noise that is not a phase marker") {
		t.Errorf("tap leaked unmatched Lima output to UI:\n%s", buf.String())
	}
}

func TestTrimLimaPrefix(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"plain text":                           "plain text",
		"INFO[0030] [hostagent] Downloading X": "Downloading X",
		"INFO[0030] Pulling: foo":              "Pulling: foo",
		"WARN[0030] [hostagent] warning":       "warning",
	}
	for in, want := range cases {
		if got := trimLimaPrefix(in); got != want {
			t.Errorf("trimLimaPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
