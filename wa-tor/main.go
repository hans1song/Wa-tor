// Command wator implements a Wa-Tor ecosystem simulation.
//
// This simulation includes creature logic (Fish and Sharks), grid management,
// parallel processing using Goroutines, and graphical visualization using the Ebiten library.
//
// Usage:
//
//	wator [flags]
//
// The flags allow configuration of the world size, population counts, and reproduction rates.
package main

import (
	crand "crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"image/color"
	"log"
	"math/rand"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
)

// Direction constants for movement logic.
const (
	NORTH = iota
	SOUTH
	EAST
	WEST
)

// coordinate represents a 2D coordinate on the grid.
type coordinate struct {
	x, y int
}

// Simulation Configuration Flags.
var (
	// nFish is the initial number of fish in the simulation.
	nFish = flag.Int("fish", 3000, "Initial # of fish.")

	// nSharks is the initial number of sharks in the simulation.
	nSharks = flag.Int("sharks", 2000, "Initial # of sharks.")

	// fBreed is the number of chronons a fish must survive to reproduce.
	fBreed = flag.Int("fbreed", 100, "# of cycles for fish to reproduce.")

	// sBreed is the number of chronons a shark must survive to reproduce.
	sBreed = flag.Int("sbreed", 150, "# of cycles for shark to reproduce.")

	// starve is the number of chronons a shark can survive without eating.
	starve = flag.Int("starve", 150, "# of cycles shark can go with feeding before dying.")

	// wwidth is the width of the toroidal world grid.
	wwidth = flag.Int("width", 900, "Width of the world (East - West).")

	// wheight is the height of the toroidal world grid.
	wheight = flag.Int("height", 600, "Height of the world (North-South).")

	// nThreads is the number of parallel threads (goroutines) to use for simulation updates.
	nThreads = flag.Int("threads", runtime.NumCPU(), "Number of threads (goroutines) to use.")

	// benchmark determines if the simulation runs in headless benchmark mode.
	benchmark = flag.Bool("benchmark", false, "Run in benchmark mode (no graphics) for timing.")

	// chronons is the total number of time steps to run in benchmark mode.
	chronons = flag.Int("chronons", 2000, "Number of chronons to run in benchmark mode.")
)

// tick is the global tick counter for the simulation.
var tick = 0

// world is the double-buffered grid storage for the current state.
var world [][]*creature

// nextWorld is the double-buffered grid storage for the next state.
var nextWorld [][]*creature

// Creature Species Constants.
const (
	FISH = iota
	SHARK
)

// Color definitions for rendering.
var (
	fishcolor  = color.RGBA{0, 255, 0, 0} // Green
	sharkcolor = color.RGBA{255, 0, 0, 0} // Red
	watercolor = color.RGBA{0, 0, 0, 0}   // Black
)

// creature represents a biological entity in the simulation (Fish or Shark).
type creature struct {
	age     int        // Current age of the creature in chronons.
	health  int        // Energy level (relevant for Sharks).
	species int        // Species type: FISH or SHARK.
	asset   color.RGBA // Color representation for the GUI.
	chronon int        // Last chronon this creature was updated (to prevent double moves).
}

// Chronon executes a single step (time step) of the simulation.
//
// This function divides the grid into horizontal strips and assigns them to
// parallel worker goroutines based on the configured thread count. It waits
// for all workers to finish before swapping the grid buffers.
//
// c is the current chronon index.
func Chronon(c int) {
	var wg sync.WaitGroup

	numGoroutines := *nThreads

	if numGoroutines <= 0 {
		numGoroutines = 1
	}

	if numGoroutines > *wheight {
		numGoroutines = *wheight
	}

	rowsPerGoroutine := *wheight / numGoroutines

	// Launch worker threads to update slices of the grid.
	for i := 0; i < numGoroutines; i++ {
		startY := i * rowsPerGoroutine
		endY := startY + rowsPerGoroutine

		if i == numGoroutines-1 {
			endY = *wheight
		}

		wg.Add(1)
		go updateSlice(c, startY, endY, &wg)
	}

	wg.Wait()

	// Swap double buffers.
	world, nextWorld = nextWorld, world

	for i := range nextWorld {
		for j := range nextWorld[i] {
			nextWorld[i][j] = nil
		}
	}
}

// updateSlice updates a specific horizontal slice of the world grid.
//
// It handles the movement, feeding, and reproduction logic for all creatures
// within the specified Y-coordinate range (startY inclusive, endY exclusive).
// It uses atomic operations to safely write to the nextWorld grid.
func updateSlice(c, startY, endY int, wg *sync.WaitGroup) {
	defer wg.Done()

	// Initialize a unique random seed for this goroutine.
	var seed int64
	var b [8]byte
	_, err := crand.Read(b[:])
	if err != nil {
		seed = time.Now().UnixNano() + int64(startY)
	} else {
		seed = int64(binary.LittleEndian.Uint64(b[:]))
	}
	r := rand.New(rand.NewSource(seed))

	var newX, newY int

	for y := startY; y < endY; y++ {
		for x := 0; x < *wwidth; x++ {

			if world[x][y] == nil {
				continue
			}

			// Copy creature data to avoid read conflicts.
			cr := *world[x][y]
			cr.age++
			cr.chronon = c

			moved := false

			switch cr.species {
			case FISH:
				// Fish behavior: Move randomly to an empty adjacent spot.
				for i := 0; i < 4; i++ {
					north, south, east, west := adjacent(x, y)
					d := r.Intn(4)
					switch (d + i) % 4 {
					case NORTH:
						newX, newY = north.x, north.y
					case SOUTH:
						newX, newY = south.x, south.y
					case EAST:
						newX, newY = east.x, east.y
					case WEST:
						newX, newY = west.x, west.y
					}

					if world[newX][newY] == nil {
						// Use atomic CAS to claim the spot in the next world state.
						if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[newX][newY])), nil, unsafe.Pointer(&cr)) {
							moved = true
							// Reproduce if old enough.
							if cr.age > 0 && cr.age%*fBreed == 0 {
								babyFish := &creature{age: 0, species: FISH, asset: fishcolor, chronon: c}
								atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(babyFish))
							}
							break
						}
					}
				}

				if !moved {
					atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(&cr))
				}

			case SHARK:
				// Shark behavior: Starve if health is depleted.
				cr.health--
				if cr.health <= 0 {
					continue
				}

				// Priority 1: Hunt for adjacent fish.
				for i := 0; i < 4; i++ {
					north, south, east, west := adjacent(x, y)
					d := r.Intn(4)
					switch (d + i) % 4 {
					case NORTH:
						newX, newY = north.x, north.y
					case SOUTH:
						newX, newY = south.x, south.y
					case EAST:
						newX, newY = east.x, east.y
					case WEST:
						newX, newY = west.x, west.y
					}

					if world[newX][newY] != nil && world[newX][newY].species == FISH {
						cr.health = *starve
						if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[newX][newY])), nil, unsafe.Pointer(&cr)) {
							moved = true
							// Reproduce if old enough (split energy).
							if cr.age > 0 && cr.age%*sBreed == 0 {
								childEnergy := cr.health / 2
								cr.health -= childEnergy

								babyShark := &creature{age: 0, health: childEnergy, species: SHARK, asset: sharkcolor, chronon: c}
								atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(babyShark))
							}
							break
						}
					}
				}

				if moved {
					continue
				}

				// Priority 2: Move to an empty adjacent square if no fish found.
				for i := 0; i < 4; i++ {
					north, south, east, west := adjacent(x, y)
					d := r.Intn(4)
					switch (d + i) % 4 {
					case NORTH:
						newX, newY = north.x, north.y
					case SOUTH:
						newX, newY = south.x, south.y
					case EAST:
						newX, newY = east.x, east.y
					case WEST:
						newX, newY = west.x, west.y
					}

					if world[newX][newY] == nil {
						if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[newX][newY])), nil, unsafe.Pointer(&cr)) {
							moved = true
							if cr.age > 0 && cr.age%*sBreed == 0 {
								childEnergy := cr.health / 2
								cr.health -= childEnergy

								babyShark := &creature{age: 0, health: childEnergy, species: SHARK, asset: sharkcolor, chronon: c}
								atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(babyShark))
							}
							break
						}
					}
				}

				if !moved {
					atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(&cr))
				}
			}
		}
	}
}

// adjacent calculates adjacent coordinates wrapping around the toroidal world.
// It returns the north, south, east, and west coordinates relative to (x, y).
func adjacent(x, y int) (coordinate, coordinate, coordinate, coordinate) {
	var n, s, e, w coordinate
	if y == 0 {
		n.y = *wheight - 1
	} else {
		n.y = y - 1
	}
	n.x = x
	if y == *wheight-1 {
		s.y = 0
	} else {
		s.y = y + 1
	}
	s.x = x
	if x == *wwidth-1 {
		e.x = 0
	} else {
		e.x = x + 1
	}
	e.y = y
	if x == 0 {
		w.x = *wwidth - 1
	} else {
		w.x = x - 1
	}
	w.y = y

	return n, s, e, w
}

// initWator initializes the simulation world.
//
// It allocates memory for the grid and randomly populates it with the specified
// number of fish and sharks. It returns two grids: the initial world state and
// the empty 'next' state buffer.
func initWator() ([][]*creature, [][]*creature) {

	var wm = make([][]*creature, *wwidth)
	for i := range wm {
		wm[i] = make([]*creature, *wheight)
	}
	var nwm = make([][]*creature, *wwidth)
	for i := range nwm {
		nwm[i] = make([]*creature, *wheight)
	}

	pop := 0
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < *nFish; i++ {
		for {
			if pop == *wwidth**wheight {
				break
			}
			x := r.Intn(*wwidth - 1)
			y := r.Intn(*wheight - 1)

			if wm[x][y] == nil {
				wm[x][y] = &creature{
					age:     rand.Intn(*fBreed),
					species: FISH,
					asset:   fishcolor,
				}
				pop++
				break
			}
		}
	}

	for i := 0; i < *nSharks; i++ {
		for {
			if pop == *wwidth**wheight {
				break
			}
			x := r.Intn(*wwidth - 1)
			y := r.Intn(*wheight - 1)

			if wm[x][y] == nil {
				wm[x][y] = &creature{
					age:     rand.Intn(*sBreed),
					species: SHARK,
					health:  *starve,
					asset:   sharkcolor,
				}
				pop++
				break
			}
		}
	}

	return wm, nwm
}

// debug helps to print the grid to console (unused in production).
func debug() {
	for y := 0; y < *wheight; y++ {
		for x := 0; x < *wwidth; x++ {
			if world[x][y] == nil {
				fmt.Print(" ")
			} else {
				switch world[x][y].species {
				case FISH:
					fmt.Print("F")
				case SHARK:
					fmt.Print("S")
				}
			}
		}
		fmt.Println()
	}
}

// Game is a struct implementing the Ebiten interface.
type Game struct{}

// Update updates the game state. It is called every frame (tick).
func (g *Game) Update() error {
	tick++
	Chronon(tick)
	return nil
}

// Draw draws the current game state to the screen.
func (g *Game) Draw(screen *ebiten.Image) {
	screen.Fill(watercolor)
	for x := 0; x < *wwidth; x++ {
		for y := 0; y < *wheight; y++ {
			if world[x][y] != nil {
				screen.Set(x, y, world[x][y].asset)
			} else {
				screen.Set(x, y, watercolor)
			}
		}
	}
	ebitenutil.DebugPrint(screen, strconv.Itoa(tick))
}

// Layout defines the screen layout.
// It returns the internal logical screen dimensions.
func (g *Game) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	return *wwidth, *wheight
}

// main is the entry point.
//
// It parses flags, initializes the simulation, and starts either the benchmark
// or the graphical loop.
func main() {
	flag.Parse()

	if *nFish+*nSharks > *wwidth**wheight {
		log.Fatal("Not enough space for Fish and Shark!")
	}

	// Set process limits for accurate benchmarking and parallel execution.
	runtime.GOMAXPROCS(*nThreads)

	if *benchmark {
		// Headless benchmark mode.
		fmt.Printf("Running Wa-Tor benchmark...\n")
		fmt.Printf("Config: Threads=%d, Chronons=%d, Width=%d, Height=%d, Fish=%d, Sharks=%d\n",
			*nThreads, *chronons, *wwidth, *wheight, *nFish, *nSharks)

		world, nextWorld = initWator()

		startTime := time.Now()

		for i := 0; i < *chronons; i++ {
			Chronon(i)
		}

		duration := time.Since(startTime)

		fmt.Printf("--- Benchmark Complete ---\n")
		fmt.Printf("Total time for %d chronons with %d threads: %v\n", *chronons, *nThreads, duration)

	} else {
		// Interactive Ebiten GUI mode.
		world, nextWorld = initWator()
		ebiten.SetWindowSize(900, 600)
		ebiten.SetWindowTitle("Wator")

		if err := ebiten.RunGame(&Game{}); err != nil {
			log.Fatal(err)
		}
	}
}
