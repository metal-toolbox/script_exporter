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
	Scripts []*Script `yaml:"scripts"`
	Metrics []*Metric `yaml:"metrics"`
}

type Script struct {
	Name    string `yaml:"name"`
	Content string `yaml:"script"`
	Timeout int64  `yaml:"timeout"`
}

type Metric struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

type Measurement struct {
	Script   *Script
	Success  int
	Duration float64
	Labels   map[string]string
}

var pidRE = regexp.MustCompile(`LABEL:(?P<NAME>\w+):(?P<VALUE>.+)`)

func getLabels(output string) map[string]string {
	m := make(map[string]string)
	for _, v := range strings.Split(output, "\n") {
		entryMatches := pidRE.FindStringSubmatch(v)
		if entryMatches == nil {
			return m
		}
		m[entryMatches[1]] = entryMatches[2]
	}
	return m
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

			labels := getLabels(stdout)
			m.Labels = labels

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
		labels := ""
		for k, v := range measurement.Labels {
			labels += fmt.Sprintf("\"%s=%s\" ", k, v)
		}
		fmt.Fprintf(w, "script_duration_seconds{script=\"%s\"} %f\n", measurement.Script.Name, measurement.Duration)
		fmt.Fprintf(w, "script_success{script=\"%s\" %s} %d\n", measurement.Script.Name, labels, measurement.Success)
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
