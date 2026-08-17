package audit

import "testing"

func TestClassifyOutputSpeed(t *testing.T) {
	tests := []struct {
		name                    string
		output, first, duration int64
		soft, hard              float64
		minGen                  int64
		failClosed              bool
		wantClass               string
	}{
		{name: "hard", output: 2000, first: 200, duration: 1200, soft: 500, hard: 1000, minGen: 1000, wantClass: DegradeClassHard},
		{name: "soft", output: 600, first: 200, duration: 1400, soft: 500, hard: 1000, minGen: 1000, wantClass: DegradeClassSoft},
		{name: "short window follows hard threshold by default", output: 200, first: 50, duration: 250, soft: 500, hard: 1000, minGen: 1000, wantClass: DegradeClassHard},
		{name: "burst short window when fail closed", output: 200, first: 50, duration: 250, soft: 500, hard: 1000, minGen: 1000, failClosed: true, wantClass: DegradeClassBurst},
		{name: "healthy", output: 100, first: 200, duration: 2200, soft: 500, hard: 1000, minGen: 1000, wantClass: ""},
		{name: "zero generation", output: 100, first: 500, duration: 500, soft: 500, hard: 1000, minGen: 1000, wantClass: ""},
		{name: "late first token uses full duration", output: 1511, first: 19763, duration: 19827, soft: 500, hard: 1000, minGen: 1000, wantClass: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			class, tps, genMS := ClassifyOutputSpeed(test.output, test.first, test.duration, test.soft, test.hard, test.minGen, test.failClosed)
			if class != test.wantClass {
				t.Fatalf("class = %q, want %q", class, test.wantClass)
			}
			if test.name == "late first token uses full duration" {
				if genMS != test.duration {
					t.Fatalf("genMS = %d, want duration %d", genMS, test.duration)
				}
				want := float64(test.output) * 1000 / float64(test.duration)
				if tps != want {
					t.Fatalf("tps = %v, want %v", tps, want)
				}
			}
		})
	}
}

func TestGenerationWindowMSFallsBackWhenTailShorterThanWait(t *testing.T) {
	if got := GenerationWindowMS(19763, 19827); got != 19827 {
		t.Fatalf("late first token window = %d", got)
	}
	if got := GenerationWindowMS(200, 2200); got != 2000 {
		t.Fatalf("normal window = %d", got)
	}
	if got := GenerationWindowMS(50, 250); got != 200 {
		t.Fatalf("short-but-not-late window = %d", got)
	}
	if got := GenerationWindowMS(500, 500); got != 0 {
		t.Fatalf("zero window = %d", got)
	}
	if got := GenerationWindowMS(2000, 3500); got != 1500 {
		t.Fatalf("think-then-write window must keep the tail: %d", got)
	}
	if got := GenerationWindowMS(400, 1500); got != 1100 {
		t.Fatalf("tail at least 1s keeps after-first-token window: %d", got)
	}
}
