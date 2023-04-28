package main

import (
	"bytes"
	"context"
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

func runScript(script *Script) ([]MetricOutput, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(script.Timeout)*time.Second)
	defer cancel()

	bashCmd := exec.CommandContext(ctx, *shell)

	var stdBuffer bytes.Buffer
	mw := io.MultiWriter(os.Stdout, &stdBuffer)
	bashCmd.Stdout = mw
	bashCmd.Stderr = mw
	bashIn, err := bashCmd.StdinPipe()

	if err != nil {
		return []MetricOutput{}, err
	}

	if err = bashCmd.Start(); err != nil {
		return []MetricOutput{}, err
	}

	if _, err = bashIn.Write([]byte(script.Content)); err != nil {
		return []MetricOutput{}, err
	}

	bashIn.Close()

	err = bashCmd.Wait()

	return getMetricOutput(stdBuffer.String()), err
}

func runScriptWorker(config *Config) error {
	for _, s := range config.Scripts {
		go func(s *Script) {
			tickChan := time.NewTicker(time.Second * time.Duration(s.Interval)).C
			for {
				select {
				case <-tickChan:
					mos, err := runScript(s)
					if err != nil {
						continue
					}
					for _, mo := range mos {
						if m, ok := config.Metrics[mo.Name]; ok {
							processMetric(m, mo)
						} else {
							log.Infof("invalid metric name: %s", mo.Name)
						}
					}
				}
			}
		}(s)
	}
	return nil
}

func processMetric(m *Metric, mo MetricOutput) error {
	switch m.Type {
	case "GaugeVec":
		metric, ok := m.Metric.(*prometheus.GaugeVec)
		if !ok {
			log.Infof("%v is not a GaugeVec", m)
		}
		if r, err := strconv.ParseFloat(mo.Result, 64); err == nil {
			metric.WithLabelValues(mo.Labels...).Set(r)
		}
	}

	return nil
}

func createMetrics(metrics map[string]*Metric) {
	c := []prometheus.Collector{}
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
			c = append(c, m.Metric.(*prometheus.GaugeVec))
		}
	}
	prometheus.DefaultRegisterer.MustRegister(c...)
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
	go func() {
		runScriptWorker(&config)
	}()

	http.Handle("/metrics", promhttp.Handler())

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
