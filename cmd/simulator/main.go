// Command simulator is a Modbus TCP server that emulates a gas city-gate
// station: pressures, temperature, a leak alarm, isolation valves and a
// slam-shut valve, all of which respond to Modbus write commands.
//
// Read map:
//
//	Holding register 0: P_IN        (~250.0 PSI, x10)
//	Holding register 1: P_OUT       (follows the setpoint, ~60.0 PSI, x10)
//	Holding register 2: TEMP        (~15.0 °C, x10)
//	Holding register 3: P_OUT_SP    outlet pressure setpoint (x10), writable
//	Discrete input 0: LEAK alarm
//	Discrete input 1: INLET_VLV    (true = open)
//	Discrete input 2: SLAM_SHUT    (true = armed/open, false = tripped/closed)
//	Discrete input 3: OUTLET_VLV   (true = open)
//
// Write map (immediately applied, every write is logged):
//
//	FC05 coil 10: INLET_VLV   true = open, false = close
//	FC05 coil 11: SLAM_SHUT   true = re-arm after a trip (a slam-shut cannot be tripped by hand)
//	FC05 coil 12: OUTLET_VLV  true = open, false = close
//	FC06/FC16 holding register 3: outlet pressure setpoint, 500..700 (50.0 .. 70.0 PSI)
//
// Coils 0..3 read back the same image as the discrete inputs, coils 10..12 the
// valve positions. Process model: closing the inlet valve or tripping the
// slam-shut makes the pressures decay; the outlet pressure follows the setpoint.
package main

import (
	"flag"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mb "github.com/simonvetter/modbus"
)

const (
	numHolding = 4
	numBits    = 4

	coilInlet  = 10
	coilSlam   = 11
	coilOutlet = 12
	regSetpt   = 3

	setpointMin = 500 // 50.0 PSI
	setpointMax = 700 // 70.0 PSI
	lag         = 0.2 // first-order lag per tick of the pressures
)

// station holds the simulated process state behind a mutex.
type station struct {
	mu      sync.RWMutex
	holding [numHolding]uint16
	leak    bool
	inlet   bool    // inlet isolation valve open
	slam    bool    // slam-shut armed (open); false after a trip
	outlet  bool    // outlet isolation valve open
	rate    float64 // multiplier for how often events (leak, trips) start
	auto    bool    // unattended mode: valves operate randomly and the slam-shut re-arms itself

	pIn, pOut float64 // continuous process state, tenths of PSI
	setpoint  uint16  // outlet pressure setpoint, tenths of PSI
}

func newStation(rate float64, unattended bool) *station {
	return &station{inlet: true, slam: true, outlet: true, rate: rate, auto: unattended, pIn: 2500, pOut: 600, setpoint: 600}
}

// bits returns the discrete-input image. Caller holds s.mu.
func (s *station) bits() [numBits]bool {
	return [numBits]bool{s.leak, s.inlet, s.slam, s.outlet}
}

// coil returns the value of a coil address, or false if it does not exist.
// Caller holds s.mu.
func (s *station) coil(addr int) (bool, bool) {
	switch {
	case addr >= 0 && addr < numBits:
		return s.bits()[addr], true
	case addr == coilInlet:
		return s.inlet, true
	case addr == coilSlam:
		return s.slam, true
	case addr == coilOutlet:
		return s.outlet, true
	}
	return false, false
}

func onOff(v bool, on, off string) string {
	if v {
		return on
	}
	return off
}

// HandleCoils serves FC01 (read), FC05 and FC15 (write).
func (s *station) HandleCoils(req *mb.CoilsRequest) ([]bool, error) {
	if !req.IsWrite {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := make([]bool, 0, req.Quantity)
		for i := 0; i < int(req.Quantity); i++ {
			v, ok := s.coil(int(req.Addr) + i)
			if !ok {
				return nil, mb.ErrIllegalDataAddress
			}
			out = append(out, v)
		}
		return out, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// validate the whole request before applying anything (FC15 is all or nothing)
	for i := range req.Args {
		switch a := int(req.Addr) + i; a {
		case coilInlet, coilOutlet:
		case coilSlam:
			if !req.Args[i] {
				log.Printf("[SIM] WRITE coil %d <- 0 from %s REFUSED: a slam-shut cannot be tripped by command", a, req.ClientAddr)
				return nil, mb.ErrIllegalDataValue
			}
		default:
			log.Printf("[SIM] WRITE coil %d from %s REFUSED: not a writable address", a, req.ClientAddr)
			return nil, mb.ErrIllegalDataAddress
		}
	}
	for i, v := range req.Args {
		switch a := int(req.Addr) + i; a {
		case coilInlet:
			s.inlet = v
			log.Printf("[SIM] WRITE FC05 coil %d <- %d from %s: INLET valve %s", a, b2i(v), req.ClientAddr, onOff(v, "OPENED", "CLOSED"))
		case coilOutlet:
			s.outlet = v
			log.Printf("[SIM] WRITE FC05 coil %d <- %d from %s: OUTLET valve %s", a, b2i(v), req.ClientAddr, onOff(v, "OPENED", "CLOSED"))
		case coilSlam:
			if s.slam {
				log.Printf("[SIM] WRITE FC05 coil %d <- 1 from %s: SLAM-SHUT already armed", a, req.ClientAddr)
			} else {
				s.slam = true
				log.Printf("[SIM] WRITE FC05 coil %d <- 1 from %s: SLAM-SHUT trip RESET, valve re-armed", a, req.ClientAddr)
			}
		}
	}
	return nil, nil
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *station) HandleDiscreteInputs(req *mb.DiscreteInputsRequest) ([]bool, error) {
	if int(req.Addr)+int(req.Quantity) > numBits {
		return nil, mb.ErrIllegalDataAddress
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.bits()
	return append([]bool(nil), b[req.Addr:int(req.Addr)+int(req.Quantity)]...), nil
}

// HandleHoldingRegisters serves FC03 (read), FC06 and FC16 (write).
func (s *station) HandleHoldingRegisters(req *mb.HoldingRegistersRequest) ([]uint16, error) {
	if int(req.Addr)+int(req.Quantity) > numHolding && !req.IsWrite {
		return nil, mb.ErrIllegalDataAddress
	}
	if !req.IsWrite {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := make([]uint16, req.Quantity)
		copy(out, s.holding[req.Addr:int(req.Addr)+int(req.Quantity)])
		return out, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Addr != regSetpt || len(req.Args) != 1 {
		log.Printf("[SIM] WRITE holding register %d (x%d) from %s REFUSED: only register %d is writable", req.Addr, len(req.Args), req.ClientAddr, regSetpt)
		return nil, mb.ErrIllegalDataAddress
	}
	v := req.Args[0]
	if v < setpointMin || v > setpointMax {
		log.Printf("[SIM] WRITE holding register %d <- %d from %s REFUSED: setpoint must be %d..%d", req.Addr, v, req.ClientAddr, setpointMin, setpointMax)
		return nil, mb.ErrIllegalDataValue
	}
	s.setpoint = v
	s.holding[regSetpt] = v
	log.Printf("[SIM] WRITE FC06 holding register %d <- %d from %s: outlet pressure SETPOINT now %.1f PSI", req.Addr, v, req.ClientAddr, float64(v)/10)
	return append([]uint16(nil), v), nil
}

func (s *station) HandleInputRegisters(req *mb.InputRegistersRequest) ([]uint16, error) {
	return nil, mb.ErrIllegalFunction
}

func toReg(v float64) uint16 { return uint16(math.Min(math.Max(math.Round(v), 0), math.MaxUint16)) }

// step advances the process model by one tick at time t (seconds).
func (s *station) step(t float64, rng *rand.Rand) {
	jitter := func(amp float64) float64 { return (rng.Float64()*2 - 1) * amp }

	s.mu.Lock()
	defer s.mu.Unlock()

	// Pressures follow their targets with a first-order lag: they decay towards
	// zero when gas supply is cut (inlet closed or slam-shut tripped).
	targetIn, targetOut := 0.0, 0.0
	if s.inlet {
		targetIn = 2500 + 18*math.Sin(t/9)
	}
	if s.inlet && s.slam {
		targetOut = float64(s.setpoint) + 7*math.Sin(t/5+1)
	}
	s.pIn += (targetIn - s.pIn) * lag
	s.pOut += (targetOut - s.pOut) * lag
	noisy := func(v, amp float64) float64 {
		if v < 50 {
			return v
		}
		return v + jitter(amp)
	}
	temp := 150 + 8*math.Sin(t/30+2) + jitter(1.5) // 14.2 .. 15.8 °C
	s.holding[0] = toReg(noisy(s.pIn, 4))
	s.holding[1] = toReg(noisy(s.pOut, 2))
	s.holding[2] = toReg(temp)
	s.holding[regSetpt] = s.setpoint

	// Leak alarm: rare onset, clears after a few seconds.
	if s.leak {
		if rng.Float64() < 0.12 {
			s.leak = false
			log.Printf("[SIM] LEAK alarm cleared")
		}
	} else if rng.Float64() < 0.01*s.rate {
		s.leak = true
		log.Printf("[SIM] LEAK alarm raised")
	}

	// Slam-shut: trips now and then and stays tripped until a RESET command
	// (or, in unattended mode, until the simulated operator re-arms it).
	if s.slam {
		if rng.Float64() < 0.006*s.rate {
			s.slam = false
			log.Printf("[SIM] SLAM-SHUT tripped (waiting for a reset command)")
		}
	} else if s.auto && rng.Float64() < 0.07 {
		s.slam = true
		log.Printf("[SIM] SLAM-SHUT re-armed (unattended mode)")
	}
	if s.auto { // unattended: the outlet valve is operated now and then
		if s.outlet {
			if rng.Float64() < 0.002*s.rate {
				s.outlet = false
				log.Printf("[SIM] OUTLET valve closed (unattended mode)")
			}
		} else if rng.Float64() < 0.06 {
			s.outlet = true
			log.Printf("[SIM] OUTLET valve opened (unattended mode)")
		}
	}
}

func main() {
	addr := flag.String("listen", "0.0.0.0:1502", "Modbus TCP listen address")
	interval := flag.Duration("interval", 500*time.Millisecond, "process update interval")
	eventRate := flag.Float64("event-rate", 1, "multiplier for how often leaks and slam-shut trips start (try 5-10 for demos)")
	unattended := flag.Bool("unattended", false, "operate the outlet valve randomly and re-arm the slam-shut by itself (no operator needed)")
	flag.Parse()

	st := newStation(*eventRate, *unattended)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	st.step(0, rng)

	srv, err := mb.NewServer(&mb.ServerConfiguration{
		URL:        "tcp://" + *addr,
		Timeout:    30 * time.Second,
		MaxClients: 16,
	}, st)
	if err != nil {
		log.Fatalf("[SIM] create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		log.Fatalf("[SIM] start server on %s: %v", *addr, err)
	}
	log.Printf("[SIM] Modbus TCP simulator listening on %s (update every %s, unattended=%t)", *addr, *interval, *unattended)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-stop:
			log.Printf("[SIM] shutting down")
			if err := srv.Stop(); err != nil {
				log.Printf("[SIM] stop: %v", err)
			}
			return
		case <-ticker.C:
			st.step(time.Since(start).Seconds(), rng)
		}
	}
}
