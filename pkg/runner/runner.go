package runner

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/naabu/pkg/scan"
	"github.com/remeh/sizedwaitgroup"
)

// Runner is an instance of the port enumeration
// client used to orchestrate the whole process.
type Runner struct {
	options *Options
	scanner *scan.Scanner
}

// NewRunner creates a new runner struct instance by parsing
// the configuration options, configuring sources, reading lists, etc
func NewRunner(options *Options) (*Runner, error) {
	runner := &Runner{
		options: options,
	}

	scanner, err := scan.NewScanner(&scan.Options{
		Timeout: time.Duration(options.Timeout) * time.Millisecond,
		Retries: options.Retries,
		Rate:    options.Rate,
		Debug:   options.Debug,
	})
	if err != nil {
		return nil, err
	}
	runner.scanner = scanner

	runner.scanner.Ports, err = ParsePorts(options)
	if err != nil {
		return nil, fmt.Errorf("could not parse ports: %s", err)
	}

	err = runner.parseProbesPorts(options)
	if err != nil {
		return nil, fmt.Errorf("could not parse probes: %s", err)
	}

	runner.scanner.ExcludedIps, err = parseExcludedIps(options)
	if err != nil {
		return nil, err
	}

	runner.scanner.Targets = make(map[string]struct{})

	return runner, nil
}

func (r *Runner) SetSourceIpAndInterface() error {
	if r.options.SourceIp != "" && r.options.Interface != "" {
		r.scanner.SourceIP = net.ParseIP(r.options.SourceIp)
		var err error
		r.scanner.NetworkInterface, err = net.InterfaceByName(r.options.Interface)
		if err != nil {
			return err
		}
	}

	return fmt.Errorf("Source Ip and Interface not specified")
}

// RunEnumeration runs the ports enumeration flow on the targets specified
func (r *Runner) RunEnumeration() error {
	err := r.Load()
	if err != nil {
		return err
	}

	r.BackgroundWorkers()

	if err := r.SetSourceIpAndInterface(); err != nil {
		r.scanner.TuneSource(ExternalTargetForTune)
	}

	r.ProbeOrSkip()

	time.Sleep(time.Duration(WarmUpTime) * time.Millisecond)

	// update targets
	if len(r.scanner.ProbeResults.M) > 0 {
		r.scanner.Targets = r.scanner.ProbeResults.M
	}

	if !isRoot() {
		// Connect Scan - perform ports spray scan
		r.ConnectEnumeration()
	} else {
		// Syn Scan - Perform scan with raw sockets
		r.RawSocketEnumeration()
	}

	time.Sleep(time.Duration(2500) * time.Millisecond)

	r.scanner.State = scan.Done

	r.handleOutput()

	return nil
}

func (r *Runner) BackgroundWorkers() {
	r.scanner.StartWorkers()
}

func (r *Runner) RawSocketEnumeration() {
	r.scanner.State = scan.Scan
	swg := sizedwaitgroup.New(r.options.Rate)

	for retry := 0; retry < r.options.Retries; retry++ {
		for port := range r.scanner.Ports {
			for target := range r.scanner.Targets {
				swg.Add()
				go r.handleHostPortSyn(&swg, target, port)
			}
		}
	}

	swg.Wait()
}

func (r *Runner) ConnectEnumeration() {
	r.scanner.State = scan.Scan
	// naive algorithm - ports spray
	swg := sizedwaitgroup.New(r.options.Rate)

	for retry := 0; retry < r.options.Retries; retry++ {
		for port := range r.scanner.Ports {
			for target := range r.scanner.Targets {
				swg.Add()
				go r.handleHostPort(&swg, target, port)
			}
		}
	}

	swg.Wait()
}

func (r *Runner) handleHostPort(swg *sizedwaitgroup.SizedWaitGroup, host string, port int) {
	defer swg.Done()

	if r.scanner.ScanResults.Has(host, port) {
		return
	}

	open, err := scan.ConnectPort(host, port, time.Duration(r.options.Timeout)*time.Millisecond)
	if open && err == nil {
		r.scanner.ScanResults.AddPort(host, port)
	}
}

func (r *Runner) handleHostPortSyn(swg *sizedwaitgroup.SizedWaitGroup, host string, port int) {
	defer swg.Done()

	r.scanner.SynPortAsync(host, port)
}

func (r *Runner) handleOutput() {
	var (
		file   *os.File
		err    error
		output string
	)
	if r.options.Output != "" {
		output = r.options.Output
		// If the output format is json, append .json
		// else append .txt
		if r.options.OutputDirectory != "" {
			if r.options.JSON {
				output += ".json"
			} else {
				output += ".txt"
			}
		}
		file, err = os.Create(output)
		if err != nil {
			gologger.Errorf("Could not create file %s: %s\n", output, err)
			return
		}
		defer file.Close()
	}

	for host, ports := range r.scanner.ScanResults.M {
		gologger.Infof("Found %d ports on host %s\n", len(ports), host)

		// console output
		if r.options.JSON {
			data := JSONResult{Host: host}
			for port := range ports {
				data.Port = port
				b, err := json.Marshal(data)
				if err != nil {
					continue
				}
				gologger.Silentf("%s\n", string(b))
			}
		} else {
			for port := range ports {
				gologger.Silentf("%s:%d\n", host, port)
			}
		}

		// file output
		if file != nil {
			if r.options.JSON {
				err = WriteJSONOutput(host, ports, file)
			} else {
				err = WriteHostOutput(host, ports, file)
			}
			if err != nil {
				gologger.Errorf("Could not write results to file %s for %s: %s\n", output, host, err)
			}
		}
	}
}
