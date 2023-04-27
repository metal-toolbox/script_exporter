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
	measurements := runScripts(config.Scripts)

	expectedLables := [][]string{{"398493840"}}
	expectedResults := map[string]struct {
		success     int
		minDuration float64
		labels      [][]string
	}{
		"success": {1, 0, [][]string{}},
		"failure": {0, 0, [][]string{}},
		"timeout": {0, 2, [][]string{}},
		"labels":  {1, 0, expectedLables},
	}

	for _, measurement := range measurements {
		expectedResult := expectedResults[measurement.Script.Name]

		if measurement.Success != expectedResult.success {
			t.Errorf("Expected result not found: %s", measurement.Script.Name)
		}

		if measurement.Duration < expectedResult.minDuration {
			t.Errorf("Expected duration %f < %f: %s", measurement.Duration, expectedResult.minDuration, measurement.Script.Name)
		}

		for i, mo := range measurement.MetricOutputs {
			for j, v := range expectedLables[i] {
				if mo.Labels[j] != v {
					t.Errorf("Expected label not found %s: %s script: %s", mo.Labels[j], v, measurement.Script.Name)
				}
			}

		}

	}
}
