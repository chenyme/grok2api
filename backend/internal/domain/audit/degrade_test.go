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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			class, _, _ := ClassifyOutputSpeed(test.output, test.first, test.duration, test.soft, test.hard, test.minGen, test.failClosed)
			if class != test.wantClass {
				t.Fatalf("class = %q, want %q", class, test.wantClass)
			}
		})
	}
}
