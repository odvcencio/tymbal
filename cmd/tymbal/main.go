// Command tymbal runs the portable Tymbal conformance tools.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync/atomic"
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
	case "soak":
		err = runLoopback(os.Args[2:], true)
	case "__native-load":
		err = nativeLoad(os.Args[2:])
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
	fmt.Fprintln(w, "  loopback [options]      run fake (default) or explicit native duplex loopback")
	fmt.Fprintln(w, "  soak [options]          native loopback; defaults to -dur 1h -load cpu,gc")
	fmt.Fprintln(w, "  report report.json...   print report records as a Markdown table")
	fmt.Fprintln(w, "  tone [options]          play a sine tone on a platform or fake host")
	fmt.Fprintln(w, "Loopback options: -rate 48000 -period 128 -periods 2 -dur 1s -delay-periods 2 -dropout -json report.json")
	fmt.Fprintln(w, "Native loopback: -host alsa|wasapi -out ID -in ID -channels 1 -max-delay-periods N -load cpu,gc -exclusive")
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

func loopback(args []string) error { return runLoopback(args, false) }

func runLoopback(args []string, soak bool) error {
	name, defaultLoad, defaultHost := "loopback", "", "fake"
	defaultDuration := time.Second
	if soak {
		name, defaultLoad, defaultHost = "soak", "cpu,gc", ""
		defaultDuration = time.Hour
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	hostName := fs.String("host", defaultHost, "host name; soak requires an explicit native host")
	outputID := fs.String("out", "", "enumerated output endpoint ID (required for native runs)")
	inputID := fs.String("in", "", "enumerated input endpoint ID (required for native runs)")
	channels := fs.Int("channels", 1, "input and output channels")
	outChannels := fs.Int("out-channels", 0, "output channels; zero uses -channels")
	inChannels := fs.Int("in-channels", 0, "input channels; zero uses -channels")
	exclusive := fs.Bool("exclusive", false, "request exclusive endpoint access")
	maxDelay := fs.Int("max-delay-periods", 0, "native latency search bound; zero uses at least 250 ms")
	rate := fs.Int("rate", 48_000, "sample rate in Hz")
	period := fs.Int("period", 128, "frames per callback")
	periods := fs.Int("periods", 2, "buffer depth in periods")
	duration := fs.Duration("dur", defaultDuration, "continuity run duration")
	delay := fs.Int("delay-periods", 2, "virtual loopback delay in periods")
	dropout := fs.Bool("dropout", false, "inject one virtual dropped period")
	dropoutAt := fs.Int("dropout-at", 0, "period for the injected dropout")
	load := fs.String("load", defaultLoad, "comma-separated load generators: cpu,gc; native runs use a child process")
	jsonPath := fs.String("json", "", "write the JSON report to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if soak && (*hostName == "" || *hostName == "fake") {
		return fmt.Errorf("soak requires an explicit native -host and -out/-in endpoint IDs")
	}
	if *outChannels == 0 {
		*outChannels = *channels
	}
	if *inChannels == 0 {
		*inChannels = *channels
	}
	cfg := tymbal.Config{SampleRate: *rate, Period: *period, Periods: *periods, OutChannels: *outChannels, InChannels: *inChannels, Exclusive: *exclusive}
	opts := tymbaltest.LoopbackOptions{
		Duration: *duration, DelayPeriods: *delay, MaxDelayPeriods: *maxDelay,
		InjectDropout: *dropout, InjectDropoutAtPeriod: *dropoutAt,
		Load: splitLoad(*load),
	}
	var record tymbaltest.Report
	var runErr error
	if *hostName == "fake" {
		if *exclusive || *outChannels <= 0 || *inChannels <= 0 {
			return fmt.Errorf("invalid or unsupported fake loopback channel/access request")
		}
		if *outputID == "" && *inputID == "" {
			record, runErr = tymbaltest.FakeLoopback(cfg, opts)
		} else {
			host, _ := tymbaltest.NewFakeHost(tymbaltest.FakeConfig{Manual: true, Loopback: true})
			out, in, err := loopbackEndpoints(host, *outputID, *inputID)
			if err != nil {
				return err
			}
			record, runErr = tymbaltest.Loopback(out, in, cfg, opts)
		}
	} else {
		// Virtual-only flags cannot be silently accepted by a physical run.
		var virtualFlag string
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "delay-periods" || f.Name == "dropout" || f.Name == "dropout-at" {
				virtualFlag = f.Name
			}
		})
		if virtualFlag != "" {
			return fmt.Errorf("-%s is only supported with -host fake", virtualFlag)
		}
		var host tymbal.Host
		for _, candidate := range tymbal.Hosts() {
			if candidate.Name() == *hostName {
				host = candidate
				break
			}
		}
		if host.Name() == "" {
			return fmt.Errorf("host %q is unavailable", *hostName)
		}
		out, in, err := loopbackEndpoints(host, *outputID, *inputID)
		if err != nil {
			return err
		}
		nativeOpts := tymbaltest.NativeOptions{Duration: *duration, MaxDelayPeriods: *maxDelay, Load: opts.Load}
		if len(opts.Load) > 0 {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			nativeOpts.LoadCommand = []string{executable, "__native-load"}
		}
		record, runErr = tymbaltest.NativeLoopback(host, out, in, cfg, nativeOpts)
	}
	if runErr != nil && record.Host == "" {
		return runErr
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
	if runErr != nil {
		return runErr
	}
	if !record.Passed {
		if record.Host == "fake" {
			return fmt.Errorf("virtual conformance checks did not pass")
		}
		return fmt.Errorf("native loopback checks did not pass")
	}
	return nil
}

func loopbackEndpoints(host tymbal.Host, outputID, inputID string) (tymbal.Device, tymbal.Device, error) {
	if outputID == "" || inputID == "" {
		return tymbal.Device{}, tymbal.Device{}, fmt.Errorf("loopback requires explicit -out and -in endpoint IDs")
	}
	devices, err := host.Devices()
	if err != nil {
		return tymbal.Device{}, tymbal.Device{}, err
	}
	var out, in tymbal.Device
	for _, device := range devices {
		if device.ID == outputID && device.Outputs > 0 {
			out = device
		}
		if device.ID == inputID && device.Inputs > 0 {
			in = device
		}
	}
	if out.ID == "" || in.ID == "" {
		return out, in, fmt.Errorf("host %q has no usable output/input pair %q/%q", host.Name(), outputID, inputID)
	}
	return out, in, nil
}

// nativeLoad runs only in a separate CLI process. Its allocations and GC never
// share the audio engine's Go runtime. The parent kills and reaps this worker.
var nativeGCSink []byte // forces the child worker's allocation onto the heap

func nativeLoad(args []string) error {
	fs := flag.NewFlagSet("__native-load", flag.ContinueOnError)
	load := fs.String("load", "", "cpu,gc")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kinds := splitLoad(*load)
	if fs.NArg() != 0 || len(kinds) == 0 {
		return fmt.Errorf("load worker requires cpu or gc")
	}
	for _, kind := range kinds {
		if kind != "cpu" && kind != "gc" {
			return fmt.Errorf("unsupported load %q", kind)
		}
	}
	var sink atomic.Uint64
	for _, kind := range kinds {
		switch kind {
		case "cpu":
			for i := 0; i < runtime.NumCPU(); i++ {
				go func(seed uint64) {
					x := seed + 1
					for {
						for j := 0; j < 4096; j++ {
							x = x*6364136223846793005 + 1
						}
						sink.Store(x)
					}
				}(uint64(i))
			}
		case "gc":
			go func() {
				for {
					nativeGCSink = make([]byte, 32*1024)
					sink.Add(uint64(len(nativeGCSink)))
					runtime.GC()
				}
			}()
		}
	}
	for {
		time.Sleep(time.Hour)
	}
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
