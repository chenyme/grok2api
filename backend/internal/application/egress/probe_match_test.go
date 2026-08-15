package egress

import "testing"

func TestMatchExpectedModes(t *testing.T) {
	text := "天空是蓝的，因为瑞利散射。\nQUALITY_OK\n"
	if !MatchExpected(text, "QUALITY_OK", MatchLastLine) {
		t.Fatal("last line QUALITY_OK should match")
	}
	if MatchExpected("hello\nNOT_OK", "QUALITY_OK", MatchLastLine) {
		t.Fatal("wrong last line must not match")
	}
	if !MatchExpected("prefix QUALITY_OK suffix", "QUALITY_OK", MatchContains) {
		t.Fatal("contains should match")
	}
	if MatchExpected("done\nstatus=QUALITY_OK", "QUALITY_OK", MatchLastLine) {
		t.Fatal("last-line mode must require the complete marker line")
	}
	if MatchExpected("done\nNOT_QUALITY_OK", "QUALITY_OK", MatchLastLine) {
		t.Fatal("last-line mode must not accept a marker substring")
	}
	if !MatchExpected("alpha\nbeta QUALITY_OK", `QUALITY_OK$`, MatchRegex) {
		t.Fatal("regex should match")
	}
	if MatchExpected("nope", "[", MatchRegex) {
		t.Fatal("invalid regex must not match")
	}
	if !MatchExpected("anything", "", MatchContains) {
		t.Fatal("empty expected is always a match")
	}
}
