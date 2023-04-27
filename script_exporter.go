package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

var (
	showVersion   = flag.Bool("version", false, "Print version information.")
	configFile    = flag.String("config.file", "script-exporter.yml", "Script exporter configuration file.")
	listenAddress = flag.String("web.listen-address", ":9172", "The address to listen on for HTTP requests.")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	shell         = flag.String("config.shell", "/bin/sh", "Shell to execute script")
)

type Config struct {
	Scripts []*Script          `yaml:"scripts"`
	Metrics map[string]*Metric `yaml:"metrics"`
}

type Script struct {
	Name     string `yaml:"name"`
	Content  string `yaml:"script"`
	Timeout  int64  `yaml:"timeout"`
	Interval int    `yaml:"interval"`
}

type Metric struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	Help      string   `yaml:"help"`
	Labels    []string `yaml:"labels"`
	Namespace string   `yaml:"namespace"`
	Metric    interface{}
}

type MetricOutput struct {
	Name   string
	Result string
	Labels []string
}

type Measurement struct {
	Script        *Script
	Success       int
	Duration      float64
	MetricOutputs []MetricOutput
}

var pidRE = regexp.MustCompile(`NAME:(?P<NAME>\w+):LABEL_VALUES:(?P<VALUE>.+):RESULT:(?P<VALUE>.+)`)

func getMetricOutput(output string) []MetricOutput {
	ms := []MetricOutput{}
	for _, v := range strings.Split(output, "\n") {
		entryMatches := pidRE.FindStringSubmatch(v)
		if entryMatches == nil {
			continue
		}
		m := MetricOutput{}
		m.Name = entryMatches[1]
		m.Labels = strings.Split(entryMatches[2], ",")
		m.Result = entryMatches[3]
		ms = append(ms, m)
	}
	return ms
}

func runScript(script *Script) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(script.Timeout)*time.Second)
	defer cancel()

	bashCmd := exec.CommandContext(ctx, *shell)

	var stdBuffer bytes.Buffer
	mw := io.MultiWriter(os.Stdout, &stdBuffer)
	bashCmd.Stdout = mw
	bashCmd.Stderr = mw
	bashIn, err := bashCmd.StdinPipe()

	if err != nil {
		return "", err
	}

	if err = bashCmd.Start(); err != nil {
		return "", err
	}

	if _, err = bashIn.Write([]byte(script.Content)); err != nil {
		return "", err
	}

	bashIn.Close()

	err = bashCmd.Wait()
	return stdBuffer.String(), err
}

func runScripts(scripts []*Script) []*Measurement {
	measurements := make([]*Measurement, 0)

	ch := make(chan *Measurement)

	for _, script := range scripts {
		go func(script *Script) {
			start := time.Now()
			success := 0
			stdout, err := runScript(script)
			duration := time.Since(start).Seconds()

			if err == nil {
				log.Debugf("OK: %s (after %fs).", script.Name, duration)
				success = 1
			} else {
				log.Infof("ERROR: %s: %s (failed after %fs).", script.Name, err, duration)
			}

			m := &Measurement{
				Script:   script,
				Duration: duration,
				Success:  success,
			}

			mo := getMetricOutput(stdout)
			m.MetricOutputs = mo

			ch <- m
		}(script)
	}

	for i := 0; i < len(scripts); i++ {
		measurements = append(measurements, <-ch)
	}

	return measurements
}

func scriptFilter(scripts []*Script, name, pattern string) (filteredScripts []*Script, err error) {
	if name == "" && pattern == "" {
		err = errors.New("`name` or `pattern` required")
		return
	}

	var patternRegexp *regexp.Regexp

	if pattern != "" {
		patternRegexp, err = regexp.Compile(pattern)

		if err != nil {
			return
		}
	}

	for _, script := range scripts {
		if script.Name == name || (pattern != "" && patternRegexp.MatchString(script.Name)) {
			filteredScripts = append(filteredScripts, script)
		}
	}

	return
}

func scriptRunHandler(w http.ResponseWriter, r *http.Request, config *Config) {
	params := r.URL.Query()
	name := params.Get("name")
	pattern := params.Get("pattern")

	scripts, err := scriptFilter(config.Scripts, name, pattern)

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	measurements := runScripts(scripts)

	for _, measurement := range measurements {

		for _, v := range measurement.MetricOutputs {
			if m, ok := config.Metrics[v.Name]; ok {

				processMetric(m, v)
			} else {
				log.Infof("invalid metric name: %s", v.Name)
			}

		}
	}
}

func processMetric(m *Metric, mo MetricOutput) error {
	switch m.Type {
	case "GaugeVec":
		metric, ok := m.Metric.(prometheus.GaugeVec)
		if !ok {
			log.Infof("%v is not a GaugeVec", m)
		}
		if r, err := strconv.ParseFloat(mo.Result, 64); err == nil {
			metric.WithLabelValues(mo.Labels...).Add(r)
		}
	}

	return nil
}

func createMetrics(metrics map[string]*Metric) {
	for _, m := range metrics {
		switch m.Type {
		case "GaugeVec":
			m.Metric = prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Name:      m.Name,
					Namespace: m.Namespace,
					Help:      m.Help,
				},
				m.Labels,
			)
		}
	}
}

func init() {
	prometheus.MustRegister(version.NewCollector("script_exporter"))
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("script_exporter"))
		os.Exit(0)
	}

	log.Infoln("Starting script_exporter", version.Info())

	yamlFile, err := ioutil.ReadFile(*configFile)

	if err != nil {
		log.Fatalf("Error reading config file: %s", err)
	}

	config := Config{}

	err = yaml.Unmarshal(yamlFile, &config)

	if err != nil {
		log.Fatalf("Error parsing config file: %s", err)
	}

	log.Infof("Loaded %d script configurations", len(config.Scripts))

	for _, script := range config.Scripts {
		if script.Timeout == 0 {
			script.Timeout = 15
		}
	}

	createMetrics(config.Metrics)
	http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		scriptRunHandler(w, r, &config)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Script Exporter</title></head>
			<body>
			<h1>Script Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})

	log.Infoln("Listening on", *listenAddress)

	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %s", err)
	}
}
