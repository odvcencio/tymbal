package tymbaltest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"m31labs.dev/tymbal"
)

// TimingReport is a timing distribution in microseconds. Percentiles are the
// upper bounds of the corresponding Stats histogram buckets. Max is the exact
// observed maximum.
type TimingReport struct {
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
	Max  float64 `json:"max"`
}

// ClockReport summarizes device position and QPC observations collected at
// the callback boundary. The frequency is the clock's native position units
// per second; QPC values are monotonic nanoseconds.
type ClockReport struct {
	Samples              uint64       `json:"observations"`
	Frequency            uint64       `json:"frequency"`
	PositionFirst        uint64       `json:"position_first"`
	PositionLast         uint64       `json:"position_last"`
	QPCFirstNano         int64        `json:"qpc_first_ns"`
	QPCLastNano          int64        `json:"qpc_last_ns"`
	QPCStepUS            TimingReport `json:"qpc_step_us"`
	PositionRateErrorPPM float64      `json:"position_rate_error_ppm"`
}

// Report is a portable snapshot of a loopback run. FakeHost results are
// marked by Host and describe deterministic virtual behavior, not hardware.
// WakeIntervalUS and WakeIntervalBuckets describe elapsed observations between
// serviced wakes, including callback, backend, recovery, and scheduling time,
// not device deadline lateness.
type Report struct {
	Host                  string       `json:"host"`
	Mode                  string       `json:"mode,omitempty"`
	Output                string       `json:"out"`
	Input                 string       `json:"in"`
	Rate                  int          `json:"rate"`
	Period                int          `json:"period"`
	Periods               int          `json:"periods"`
	InFormat              string       `json:"in_format,omitempty"`
	OutFormat             string       `json:"out_format,omitempty"`
	LatencyInUS           float64      `json:"latency_in_us"`
	LatencyOutUS          float64      `json:"latency_out_us"`
	LatencyInSource       string       `json:"latency_in_source,omitempty"`
	LatencyOutSource      string       `json:"latency_out_source,omitempty"`
	DurationSeconds       float64      `json:"duration_s"`
	Load                  []string     `json:"load"`
	Priority              string       `json:"priority"`
	DeadlineAvailable     bool         `json:"deadline_available"`
	Callbacks             uint64       `json:"callbacks"`
	Late                  uint64       `json:"late"`
	DropoutsReported      uint64       `json:"dropouts_reported"`
	BreaksDetected        uint64       `json:"breaks_detected"`
	Discontinuities       uint64       `json:"discontinuities"`
	NonZeroOutputSamples  uint64       `json:"output_nonzero_count"`
	CallbackErrors        uint64       `json:"callback_errors"`
	Breaks                []Break      `json:"breaks,omitempty"`
	LatencyMeasuredFrames int          `json:"latency_measured_frames"`
	LatencyReportedFrames int          `json:"latency_reported_frames"`
	WakeLateUS            TimingReport `json:"wake_late_us"`
	WakeIntervalUS        TimingReport `json:"wake_interval_us"`
	CallbackUS            TimingReport `json:"callback_us"`
	WakeLateBuckets       [32]uint64   `json:"wake_late_buckets"`
	WakeIntervalBuckets   [32]uint64   `json:"wake_interval_buckets"`
	CallbackBuckets       [32]uint64   `json:"callback_buckets"`
	OutputClock           ClockReport  `json:"output_clock"`
	InputClock            ClockReport  `json:"input_clock"`
	Allocs                uint64       `json:"allocs"`
	CallbackLoopAllocs    uint64       `json:"callback_loop_allocs"`
	RuntimePriority       string       `json:"runtime_priority,omitempty"`
	Go                    string       `json:"go"`
	OS                    string       `json:"os"`
	Arch                  string       `json:"arch"`
	CPU                   string       `json:"cpu"`
	Seed                  int64        `json:"seed,omitempty"`
	Passed                bool         `json:"passed"`
}

// FillTiming copies histogram percentiles and the raw maximum from Stats.
func (r *Report) FillTiming(stats tymbal.Stats) {
	r.WakeLateUS = timingReport(&stats.WakeLate, stats.WakeLateMax)
	r.WakeIntervalUS = timingReport(&stats.WakeInterval, stats.WakeIntervalMax)
	r.CallbackUS = timingReport(&stats.CallbackTime, stats.CallbackMax)
	r.WakeLateBuckets = stats.WakeLate.Snapshot().Buckets
	r.WakeIntervalBuckets = stats.WakeInterval.Snapshot().Buckets
	r.CallbackBuckets = stats.CallbackTime.Snapshot().Buckets
}

func timingReport(h *tymbal.Histogram, max time.Duration) TimingReport {
	return TimingReport{
		P50:  float64(h.Percentile(.50)) / float64(time.Microsecond),
		P99:  float64(h.Percentile(.99)) / float64(time.Microsecond),
		P999: float64(h.Percentile(.999)) / float64(time.Microsecond),
		Max:  float64(max) / float64(time.Microsecond),
	}
}

// WriteReport writes one JSON object followed by a newline.
func WriteReport(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// ReadReport decodes one JSON loopback report.
func ReadReport(r io.Reader) (Report, error) {
	var report Report
	dec := json.NewDecoder(r)
	if err := dec.Decode(&report); err != nil {
		return Report{}, err
	}
	return report, nil
}

// MarkdownTable formats report records as a compact results table.
func MarkdownTable(reports []Report) string {
	var b strings.Builder
	b.WriteString("| Host | Output | Input | Rate | Period | Duration | Callbacks | Dropouts | Breaks | Measured latency | Reported latency |\n")
	b.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, r := range reports {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %.3fs | %d | %d | %d | %d frames | %d frames |\n",
			r.Host, r.Output, r.Input, r.Rate, r.Period, r.DurationSeconds,
			r.Callbacks, r.DropoutsReported, r.BreaksDetected, r.LatencyMeasuredFrames, r.LatencyReportedFrames)
	}
	return b.String()
}
