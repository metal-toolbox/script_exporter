package main

import (
	"testing"
)

var config = &Config{
	Metrics: map[string]*Metric{
		"fake_metric": {"last-modify-time", "GaugeVec", "help", []string{}, "namespace", struct{}{}},
	},
	Scripts: []*Script{
		{"success", "exit 0", 1, 1},
		{"failure", "exit 1", 1, 1},
		{"timeout", "sleep 5", 2, 1},
		{"labels", "echo NAME:MYMETRIC:LABEL_VALUES:398493840:RESULT:1\n", 1, 1},
	},
}

func TestRunScripts(t *testing.T) {
	for _, s := range config.Scripts {
		mos, _ := runScript(s)

		expectedLables := [][]string{{"398493840"}}
		expectedResults := map[string]struct {
			success     int
			minDuration float64
			labels      [][]string
		}{
			"success":  {1, 0, [][]string{}},
			"failure":  {0, 0, [][]string{}},
			"timeout":  {0, 2, [][]string{}},
			"MYMETRIC": {1, 0, expectedLables},
		}

		for i, mo := range mos {
			expectedResult := expectedResults[mo.Name]
			for j := range mo.Labels {
				if mo.Labels[j] != expectedResult.labels[i][j] {
					t.Errorf("Expected label not found %s: %s script: %s", mo.Labels[j], expectedLables[i][j], mo.Name)
				}
			}
		}
	}
}
