package nex

import (
	"math"
	"sync"
	"time"
)

const (
	alpha float64 = 1.0 / 8.0
	beta  float64 = 1.0 / 4.0
	k     float64 = 4.0
)

// RTT is an implementation of rdv::RTT.
// Used to calculate the average round trip time of reliable packets
type RTT struct {
	sync.Mutex
	lastRTT     float64
	average     float64
	variance    float64
	initialized bool

	// Local addition (Revivetendo): the plain smoothed round trip time, its minimum and how many
	// samples they rest on. `average` above is really a retransmission timeout (smoothed RTT plus
	// four times the variance, three times the first sample), which is the wrong number to report
	// as "how far away is this player".
	smoothed float64
	min      float64
	samples  int
}

// Adjust updates the average RTT with the new value
func (rtt *RTT) Adjust(next time.Duration) {
	// * This calculation comes from the RFC6298 which defines RTT calculation for TCP packets
	rtt.Lock()
	if rtt.initialized {
		rtt.variance = (1.0-beta)*rtt.variance + beta*math.Abs(rtt.variance-float64(next))
		rtt.average = (1.0-alpha)*rtt.average + alpha*float64(next)
	} else {
		rtt.lastRTT = float64(next)
		rtt.variance = float64(next) / 2
		rtt.average = float64(next) + k*rtt.variance
		rtt.initialized = true
	}
	rtt.Unlock()
}

// GetRTTSmoothedAvg returns the smoothed average of this RTT, it is used in calls to the custom
// RTO calculation function set on `PRUDPEndpoint::SetCalcRetransmissionTimeoutCallback`
func (rtt *RTT) GetRTTSmoothedAvg() float64 {
	return rtt.average / 16
}

// GetRTTSmoothedDev returns the smoothed standard deviation of this RTT, it is used in calls to the custom
// RTO calculation function set on `PRUDPEndpoint::SetCalcRetransmissionTimeoutCallback`
func (rtt *RTT) GetRTTSmoothedDev() float64 {
	return rtt.variance / 8
}

// Initialized returns a bool indicating whether this RTT has been initialized
func (rtt *RTT) Initialized() bool {
	return rtt.initialized
}

// GetRTO returns the current average
func (rtt *RTT) Average() time.Duration {
	return time.Duration(rtt.average)
}

// NewRTT returns a new RTT based on the first value
func NewRTT() *RTT {
	return &RTT{}
}

// Smoothed returns the plain smoothed round trip time (alpha 1/8), its minimum, and the number of
// samples they rest on (0 = none yet).
func (rtt *RTT) Smoothed() (smoothed, min time.Duration, samples int) {
	rtt.Lock()
	defer rtt.Unlock()
	return time.Duration(rtt.smoothed), time.Duration(rtt.min), rtt.samples
}

// Observe records one plain round trip sample: the time between sending a packet for the first time
// and receiving its acknowledgement (or a ping and its ack). Retransmitted packets must not be passed
// in, since an ack for them could belong to either transmission (Karn's rule).
//
// Adjust is not used for this: upstream feeds it ONLY packets that were sent at least
// RTTRetransmit (2) times, so on a healthy connection it never sees a sample, and the ones it does
// see are the ambiguous ones.
func (rtt *RTT) Observe(next time.Duration) {
	rtt.Lock()
	defer rtt.Unlock()
	if rtt.samples == 0 {
		rtt.smoothed, rtt.min = float64(next), float64(next)
	} else {
		rtt.smoothed = (1.0-alpha)*rtt.smoothed + alpha*float64(next)
		if float64(next) < rtt.min {
			rtt.min = float64(next)
		}
	}
	rtt.samples++
}
