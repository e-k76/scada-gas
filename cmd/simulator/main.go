// Command simulator is a Modbus TCP server that emulates a gas city-gate
// station: inlet/outlet pressure, gas temperature and a leak alarm.
//
// Register map (all values scaled x10):
//
//	Holding register 0: P_IN  (~250.0 PSI)
//	Holding register 1: P_OUT (~60.0 PSI)
//	Holding register 2: TEMP  (~15.0 °C)
//	Coil / discrete input 0: LEAK alarm
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
	numHolding = 3
	numBits    = 1
)

// station holds the simulated process state behind a mutex.
type station struct {
	mu      sync.RWMutex
	holding [numHolding]uint16
	leak    bool
}

func (s *station) HandleCoils(req *mb.CoilsRequest) ([]bool, error) {
	if req.IsWrite {
		return nil, mb.ErrIllegalFunction
	}
	if int(req.Addr)+int(req.Quantity) > numBits {
		return nil, mb.ErrIllegalDataAddress
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return []bool{s.leak}[req.Addr : int(req.Addr)+int(req.Quantity)], nil
}

func (s *station) HandleDiscreteInputs(req *mb.DiscreteInputsRequest) ([]bool, error) {
	if int(req.Addr)+int(req.Quantity) > numBits {
		return nil, mb.ErrIllegalDataAddress
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return []bool{s.leak}[req.Addr : int(req.Addr)+int(req.Quantity)], nil
}

func (s *station) HandleHoldingRegisters(req *mb.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		return nil, mb.ErrIllegalFunction
	}
	if int(req.Addr)+int(req.Quantity) > numHolding {
		return nil, mb.ErrIllegalDataAddress
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]uint16, req.Quantity)
	copy(out, s.holding[req.Addr:int(req.Addr)+int(req.Quantity)])
	return out, nil
}

func (s *station) HandleInputRegisters(req *mb.InputRegistersRequest) ([]uint16, error) {
	return nil, mb.ErrIllegalFunction
}

// step advances the process model by one tick at time t (seconds).
func (s *station) step(t float64, rng *rand.Rand) {
	jitter := func(amp float64) float64 { return (rng.Float64()*2 - 1) * amp }

	pIn := 2500 + 18*math.Sin(t/9) + jitter(4)     // 248.2 .. 251.8 PSI
	pOut := 600 + 7*math.Sin(t/5+1) + jitter(2)    // 59.1 .. 60.9 PSI
	temp := 150 + 8*math.Sin(t/30+2) + jitter(1.5) // 14.2 .. 15.8 °C

	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding[0] = uint16(math.Round(pIn))
	s.holding[1] = uint16(math.Round(pOut))
	s.holding[2] = uint16(math.Round(temp))

	// Leak alarm: rare onset, clears after a few seconds.
	if s.leak {
		if rng.Float64() < 0.12 {
			s.leak = false
			log.Printf("[SIM] LEAK alarm cleared")
		}
	} else if rng.Float64() < 0.01 {
		s.leak = true
		log.Printf("[SIM] LEAK alarm raised")
	}
}

func main() {
	addr := flag.String("listen", "0.0.0.0:1502", "Modbus TCP listen address")
	interval := flag.Duration("interval", 500*time.Millisecond, "process update interval")
	flag.Parse()

	st := &station{}
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
	log.Printf("[SIM] Modbus TCP simulator listening on %s (update every %s)", *addr, *interval)

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
