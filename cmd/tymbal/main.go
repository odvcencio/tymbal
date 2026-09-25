// Command tymbal runs the portable Tymbal conformance tools.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"m31labs.dev/tymbal"
	"m31labs.dev/tymbal/tymbaltest"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "loopback":
		err = loopback(os.Args[2:])
	case "devices":
		err = devices()
	case "report":
		err = report(os.Args[2:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tymbal:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: tymbal <command> [options]")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  devices                 list virtual devices")
	fmt.Fprintln(w, "  loopback [options]      run the deterministic FakeHost conformance harness")
	fmt.Fprintln(w, "  report report.json...   print report records as a Markdown table")
	fmt.Fprintln(w, "Loopback options: -rate 48000 -period 128 -periods 2 -dur 1s -delay-periods 2 -dropout -json report.json")
}

func devices() error {
	host, _ := tymbaltest.NewFakeHost(tymbaltest.FakeConfig{Manual: true, Loopback: true})
	list, err := host.Devices()
	if err != nil {
		return err
	}
	for _, d := range list {
		fmt.Printf("%s\t%s\tin=%d\tout=%d\thost=%s\n", d.ID, d.Name, d.Inputs, d.Outputs, d.Host)
	}
	return nil
}

func loopback(args []string) error {
	fs := flag.NewFlagSet("loopback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rate := fs.Int("rate", 48_000, "sample rate in Hz")
	period := fs.Int("period", 128, "frames per callback")
	periods := fs.Int("periods", 2, "buffer depth in periods")
	duration := fs.Duration("dur", time.Second, "continuity run duration")
	delay := fs.Int("delay-periods", 2, "virtual loopback delay in periods")
	dropout := fs.Bool("dropout", false, "inject one virtual dropped period")
	dropoutAt := fs.Int("dropout-at", 0, "period for the injected dropout")
	load := fs.String("load", "", "comma-separated virtual load generators: cpu,gc")
	jsonPath := fs.String("json", "", "write the JSON report to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	cfg := tymbal.Config{SampleRate: *rate, Period: *period, Periods: *periods}
	opts := tymbaltest.LoopbackOptions{
		Duration: *duration, DelayPeriods: *delay,
		InjectDropout: *dropout, InjectDropoutAtPeriod: *dropoutAt,
		Load: splitLoad(*load),
	}
	record, err := tymbaltest.FakeLoopback(cfg, opts)
	if err != nil {
		return err
	}
	if *jsonPath != "" {
		file, err := os.Create(*jsonPath)
		if err != nil {
			return err
		}
		writeErr := tymbaltest.WriteReport(file, record)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tymbaltest.WriteReport(os.Stdout, record); err != nil {
		return err
	}
	if !record.Passed {
		return fmt.Errorf("virtual conformance checks did not pass")
	}
	return nil
}

func report(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("report requires at least one JSON file")
	}
	reports := make([]tymbaltest.Report, 0, len(args))
	for _, path := range args {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		record, readErr := tymbaltest.ReadReport(file)
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("%s: %w", path, readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		reports = append(reports, record)
	}
	fmt.Print(tymbaltest.MarkdownTable(reports))
	return nil
}

func splitLoad(raw string) []string {
	if raw == "" {
		return nil
	}
	var parts []string
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == ',' {
			if i > start {
				parts = append(parts, raw[start:i])
			}
			start = i + 1
		}
	}
	return parts
}
