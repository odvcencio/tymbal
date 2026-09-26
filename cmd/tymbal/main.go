// Command tymbal runs the portable Tymbal conformance tools.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
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
	case "tone":
		err = tone(os.Args[2:])
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
	fmt.Fprintln(w, "  devices                 list platform and virtual devices")
	fmt.Fprintln(w, "  loopback [options]      run the deterministic FakeHost conformance harness")
	fmt.Fprintln(w, "  report report.json...   print report records as a Markdown table")
	fmt.Fprintln(w, "  tone [options]          play a sine tone on a platform or fake host")
	fmt.Fprintln(w, "Loopback options: -rate 48000 -period 128 -periods 2 -dur 1s -delay-periods 2 -dropout -json report.json")
	fmt.Fprintln(w, "Tone options: -host alsa|wasapi|fake -device ID -rate 48000 -period 256 -periods 2 -channels 2 -freq 997 -dur 10s -exclusive")
}

func devices() error {
	hosts := tymbal.Hosts()
	fake, _ := tymbaltest.NewFakeHost(tymbaltest.FakeConfig{Manual: true, Loopback: true})
	hosts = append(hosts, fake)
	for _, host := range hosts {
		list, err := host.Devices()
		if err != nil {
			return fmt.Errorf("%s: %w", host.Name(), err)
		}
		for _, d := range list {
			fmt.Printf("%s\t%s\tin=%d\tout=%d\thost=%s\n", d.ID, d.Name, d.Inputs, d.Outputs, d.Host)
		}
	}
	return nil
}

func tone(args []string) error {
	fs := flag.NewFlagSet("tone", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	hostName := fs.String("host", "", "platform host name, or fake")
	deviceID := fs.String("device", "", "output device ID; defaults to the host's default")
	rate := fs.Int("rate", 48_000, "sample rate in Hz")
	period := fs.Int("period", 256, "frames per callback")
	periods := fs.Int("periods", 2, "buffer depth in periods")
	channels := fs.Int("channels", 2, "output channels")
	freq := fs.Float64("freq", 997, "tone frequency in Hz")
	duration := fs.Duration("dur", 10*time.Second, "playback duration")
	exclusive := fs.Bool("exclusive", false, "request exclusive access to the endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *duration <= 0 || *freq <= 0 || *freq >= float64(*rate)/2 {
		return fmt.Errorf("invalid tone arguments")
	}
	hosts := tymbal.Hosts()
	if *hostName == "fake" {
		fake, _ := tymbal.NewFakeHost(tymbal.FakeConfig{})
		hosts = []tymbal.Host{fake}
	}
	var host tymbal.Host
	for _, candidate := range hosts {
		if *hostName == "" || candidate.Name() == *hostName {
			host = candidate
			break
		}
	}
	if host.Name() == "" {
		return fmt.Errorf("host %q is unavailable", *hostName)
	}
	var device tymbal.Device
	var err error
	if *deviceID == "" {
		device, err = host.Default(tymbal.Output)
		if err != nil {
			return err
		}
	} else {
		devices, err := host.Devices()
		if err != nil {
			return err
		}
		for _, candidate := range devices {
			if candidate.ID == *deviceID {
				device = candidate
				break
			}
		}
		if device.ID == "" {
			return fmt.Errorf("output device %q was not found", *deviceID)
		}
	}
	if *channels > device.Outputs {
		return fmt.Errorf("device %q has at most %d output channels", device.ID, device.Outputs)
	}
	config := tymbal.Config{
		Output: &device, OutChannels: *channels,
		SampleRate: *rate, Period: *period, Periods: *periods,
		Exclusive: *exclusive,
	}
	phase := 0.0
	step := 0.0
	stream, err := tymbal.Open(host, config, func(_ tymbal.Time, _, out [][]float32) {
		for frame := range out[0] {
			sample := float32(0.15 * math.Sin(phase))
			phase += step
			if phase >= 2*math.Pi {
				phase -= 2 * math.Pi
			}
			for channel := range out {
				out[channel][frame] = sample
			}
		}
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	actual := stream.Actual()
	step = 2 * math.Pi * *freq / float64(actual.SampleRate)
	if err := stream.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "host=%s device=%s rate=%d period=%d periods=%d format=%s\n", host.Name(), device.ID, actual.SampleRate, actual.Period, actual.Periods, actual.OutFormat)
	timer := time.NewTimer(*duration)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			if err := stream.Stop(); err != nil {
				return err
			}
			var stats tymbal.Stats
			stream.Stats(&stats)
			fmt.Fprintf(os.Stderr, "callbacks=%d dropouts=%d late=%d\n", stats.Callbacks, stats.Dropouts, stats.Late)
			return stream.Err()
		case <-ticker.C:
			if err := stream.Err(); err != nil {
				return err
			}
		}
	}
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
