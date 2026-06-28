package runtime

import "testing"

func TestParseStats(t *testing.T) {
	out := `{"Name":"echo-abc","CPUPerc":"1.50%","MemUsage":"10MiB / 2GiB","PIDs":"7"}
{"Name":"echo-abc-llm","CPUPerc":"0.20%","MemUsage":"5MiB / 2GiB","PIDs":"3"}

not a json line, skipped
`
	stats := parseStats(out)
	if len(stats) != 2 {
		t.Fatalf("stats = %d, want 2 (blank and non-JSON lines skipped)", len(stats))
	}
	if stats[0].Name != "echo-abc" || stats[0].CPU != "1.50%" || stats[0].Mem != "10MiB / 2GiB" || stats[0].PIDs != "7" {
		t.Errorf("row 0 = %+v", stats[0])
	}
	if stats[1].Name != "echo-abc-llm" {
		t.Errorf("row 1 = %+v", stats[1])
	}
}
