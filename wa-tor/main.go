// Package main implements a high-performance, concurrent simulation of the Wa-Tor
// predator-prey model using the Ebiten game library.
//
// Author: Chengyan Song
//
// Wa-Tor is a population dynamics simulation on a toroidal grid (wrapping edges).
// It consists of two species: Fish and Sharks.
//   - Fish move randomly and breed after a certain age.
//   - Sharks move, hunt fish for energy, breed, and die if they run out of energy (starve).
//
// The simulation uses a double-buffered grid system and employs concurrency for processing
// by utilizing distinct horizontal strips processed by separate goroutines. Synchronization
// is handled via sync.WaitGroup and atomic Compare-And-Swap (CAS) operations to
// ensure thread-safe updates to the next state buffer without heavy mutex locking.
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

// Direction constants representing the four cardinal directions.
// Used for calculating adjacent coordinates.
const (
	NORTH = iota
	SOUTH
	EAST
	WEST
)

// coordinate represents a specific (x, y) position on the 2D world grid.
type coordinate struct {
	x, y int
}

// Simulation Configuration Flags
// These variables are populated via command-line arguments at runtime.
var (
	// nFish sets the initial population of fish.
	nFish = flag.Int("fish", 3000, "Initial number of fish.")

	// nSharks sets the initial population of sharks.
	nSharks = flag.Int("sharks", 2000, "Initial number of sharks.")

	// fBreed is the number of chronons (ticks) a fish must survive to reproduce.
	fBreed = flag.Int("fbreed", 100, "Number of cycles required for a fish to reproduce.")

	// sBreed is the number of chronons (ticks) a shark must survive to reproduce.
	sBreed = flag.Int("sbreed", 150, "Number of cycles required for a shark to reproduce.")

	// starve is the initial energy level of a shark and the energy gained from eating a fish.
	// If a shark's energy reaches 0, it dies.
	starve = flag.Int("starve", 150, "Number of cycles a shark can survive without feeding before dying.")

	// wwidth is the width of the toroidal world grid (East-West axis).
	wwidth = flag.Int("width", 900, "Width of the world in cells.")

	// wheight is the height of the toroidal world grid (North-South axis).
	wheight = flag.Int("height", 600, "Height of the world in cells.")

	// nThreads determines the number of concurrent goroutines used to update the world state.
	// Defaults to the number of logical CPUs available.
	nThreads = flag.Int("threads", runtime.NumCPU(), "Number of concurrent threads (goroutines) to use.")

	// benchmark enables headless mode (no GUI) for performance testing.
	benchmark = flag.Bool("benchmark", false, "Run in benchmark mode (no graphics) for timing analysis.")

	// chronons defines the total simulation steps to execute when running in benchmark mode.
	chronons = flag.Int("chronons", 2000, "Total number of chronons (time steps) to run in benchmark mode.")
)

// Global simulation state variables.
var (
	// tick tracks the current simulation step (chronon).
	tick = 0

	// world is the current state of the simulation grid.
	// It is a read-only buffer during a specific update cycle.
	world [][]*creature

	// nextWorld is the future state of the simulation grid.
	// It is the write buffer where updates are stored during a cycle.
	nextWorld [][]*creature
)

// Species constants.
const (
	FISH = iota
	SHARK
)

// Rendering colors for grid entities.
var (
	fishcolor  = color.RGBA{0, 255, 0, 0} // Green
	sharkcolor = color.RGBA{255, 0, 0, 0} // Red
	watercolor = color.RGBA{0, 0, 0, 0}   // Transparent/Black
)

// creature represents a single entity (Agent) in the simulation.
// It can be either a Fish or a Shark.
type creature struct {
	age     int        // The number of cycles the creature has lived.
	health  int        // Energy level (only relevant for Sharks). Decreases over time, increases when eating.
	species int        // The type of creature: FISH or SHARK.
	asset   color.RGBA // The color used to render this creature.
	chronon int        // The last tick index this creature was processed (prevents double updates).
}

// Chronon advances the simulation by a single unit of time.
//
// It implements a concurrent "fork-join" pattern:
// 1. Partitioning: The world height is divided into horizontal strips based on *nThreads.
// 2. Processing: Goroutines are spawned to process each strip (updateSlice).
// 3. Synchronization: The main thread waits for all goroutines to finish via sync.WaitGroup.
// 4. Swapping: The 'nextWorld' buffer becomes the 'world' buffer for the next frame.
//
// c represents the current tick index.
func Chronon(c int) {
	var wg sync.WaitGroup

	numGoroutines := *nThreads

	// Safety check for invalid thread counts.
	if numGoroutines <= 0 {
		numGoroutines = 1
	}

	// Ensure we don't have more threads than rows.
	if numGoroutines > *wheight {
		numGoroutines = *wheight
	}

	rowsPerGoroutine := *wheight / numGoroutines

	// Launch worker threads to update slices of the grid.
	for i := 0; i < numGoroutines; i++ {
		startY := i * rowsPerGoroutine
		endY := startY + rowsPerGoroutine

		// Ensure the last routine covers any remaining rows due to integer division.
		if i == numGoroutines-1 {
			endY = *wheight
		}

		wg.Add(1)
		go updateSlice(c, startY, endY, &wg)
	}

	// Wait for all slice updates to complete.
	wg.Wait()

	// Swap double buffers.
	// The fully populated nextWorld becomes the read-only world for the next frame.
	world, nextWorld = nextWorld, world

	// Reset the new write buffer (nextWorld) to nil pointers.
	for i := range nextWorld {
		for j := range nextWorld[i] {
			nextWorld[i][j] = nil
		}
	}
}

// updateSlice processes the logic for a horizontal strip of the world.
//
// It iterates through the assigned rows (startY to endY) and applies the Wa-Tor rules:
//   - Fish: Move randomly, breed if age > fBreed.
//   - Shark: Lose energy, hunt fish, move randomly if no food, breed if age > sBreed, die if health <= 0.
//
// Concurrency Safety:
// Since creatures moving near the boundary of a slice might attempt to write to the same
// cell in 'nextWorld' as a neighbor thread, this function uses atomic.CompareAndSwapPointer
// to safely claim a target cell.
func updateSlice(c, startY, endY int, wg *sync.WaitGroup) {
	defer wg.Done()

	// Initialize a thread-local random number generator.
	// Using the global rand.Intn would require a mutex lock, slowing down concurrent execution.
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

			// Skip empty cells.
			if world[x][y] == nil {
				continue
			}

			// Copy the creature struct to a local variable.
			// We modify the copy before attempting to place it in the next world.
			cr := *world[x][y]
			cr.age++
			cr.chronon = c

			moved := false

			switch cr.species {
			case FISH:
				// --- FISH BEHAVIOR ---
				// Try to move to a random adjacent empty spot.
				for i := 0; i < 4; i++ {
					north, south, east, west := adjacent(x, y)
					d := r.Intn(4)
					// Randomize direction check order
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

					// Check if target spot in the current world is empty.
					// Note: Wa-Tor usually checks the *current* world for emptiness.
					if world[newX][newY] == nil {
						// Atomic CAS: Try to write the fish to the nextWorld slot.
						// If nextWorld[newX][newY] is not nil, another thread (or this one) already filled it.
						if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[newX][newY])), nil, unsafe.Pointer(&cr)) {
							moved = true
							// Breeding logic: Leave a new baby fish in the old spot.
							if cr.age > 0 && cr.age%*fBreed == 0 {
								babyFish := &creature{age: 0, species: FISH, asset: fishcolor, chronon: c}
								// We don't strictly need CAS here if we assume only one thing leaves a square,
								// but it's safer for correctness.
								atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(babyFish))
							}
							break
						}
					}
				}

				// If the fish couldn't move, it stays in the same spot.
				if !moved {
					atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(&cr))
				}

			case SHARK:
				// --- SHARK BEHAVIOR ---
				// 1. Metabolism: Lose energy.
				cr.health--
				if cr.health <= 0 {
					// Shark dies (we simply do not add it to nextWorld).
					continue
				}

				// 2. Hunting: Try to find a fish in adjacent cells.
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

					// If an adjacent cell has a fish, eat it.
					if world[newX][newY] != nil && world[newX][newY].species == FISH {
						cr.health = *starve // Reset energy after eating.

						// Try to move into the fish's spot (effectively eating it in the next frame).
						// Note: This simulation logic assumes "first shark to claim the spot gets the fish".
						if atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[newX][newY])), nil, unsafe.Pointer(&cr)) {
							moved = true
							// Breeding logic
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

				// 3. Movement: If no fish found, move to a random empty spot.
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
							// Breeding logic (even if just moving)
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

				// If the shark couldn't move or hunt, it stays in place.
				if !moved {
					atomic.CompareAndSwapPointer((*unsafe.Pointer)(unsafe.Pointer(&nextWorld[x][y])), nil, unsafe.Pointer(&cr))
				}
			}
		}
	}
}

// adjacent calculates the coordinates of the four neighbors of (x, y).
//
// It implements toroidal wrapping, meaning coordinates wrap around the edges of the grid:
// - Moving off the right edge (East) puts you at x=0.
// - Moving off the top edge (North) puts you at the bottom.
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

// initWator prepares the initial state of the simulation.
//
// It allocates the 2D slices for 'world' and 'nextWorld' and randomly populates
// 'world' with the requested number of Fish and Sharks at random locations.
// It ensures no two creatures occupy the same starting cell.
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

	// Populate Fish
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

	// Populate Sharks
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

// debug prints a text representation of the grid to stdout.
// Used primarily for verification during development, not in the GUI loop.
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

// Game implements the ebiten.Game interface.
type Game struct{}

// Update proceeds the game state.
// It is called every tick (usually 60 times per second by default in Ebiten).
// See: https://pkg.go.dev/github.com/hajimehoshi/ebiten/v2#Game
func (g *Game) Update() error {
	tick++
	Chronon(tick)
	return nil
}

// Draw renders the game screen.
// It iterates over the world grid and sets pixels based on the creature type.
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

// Layout accepts the outside window dimensions and returns the logical game screen size.
// Here, the logical size matches the simulation grid size exactly.
func (g *Game) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	return *wwidth, *wheight
}

// main is the application entry point.
//
// It parses command-line flags and initializes the simulation.
// Based on the 'benchmark' flag, it either:
// 1. Runs a headless simulation for performance timing.
// 2. Starts the interactive Ebiten GUI window.
func main() {
	flag.Parse()

	// Ensure the grid is large enough for the initial population.
	if *nFish+*nSharks > *wwidth**wheight {
		log.Fatal("Not enough space for Fish and Shark!")
	}

	// Set GOMAXPROCS to match the thread count for optimal concurrent execution.
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
