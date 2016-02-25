package main

// #cgo CFLAGS: -O3 -march=native -g -g3
// #include "collider_worker.h"
import "C"

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
)

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
var mainChain = flag.Bool("main", false, "mine the main chain")
var genesisDifficulty = flag.Int("difficulty", 42, "difficulty for mining genesis block")
var maxTable = flag.Int("table", 28, "log base 2 of maximum table size")

func main() {
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	var parent []byte
	if !*mainChain {
		parent, _ = hex.DecodeString("169740d5c4711f3cbbde6b9bfbbe8b3d236879d849d1c137660fce9e7884cae7")
	}
	for {
		timer := time.NewTimer(15 * time.Second)
		if *mainChain {
			mine(nil)
		} else {
			parent = mine(parent)
		}

		select {
		case <-timer.C:
		}
	}
}

func mine(parent []byte) []byte {
	runtime.GC()

	client := http.Client{
		Timeout: time.Second * 5,
	}
	blk := Block{
		Header: new(BlockHeader),
		Block:  "dpchen,lopezv,asnoakes",
	}
	if parent == nil {
		resp, err := client.Get("http://6857coin.csail.mit.edu:8080/next")
		if err != nil {
			log.Println(err)
			return nil
		}
		dec := json.NewDecoder(resp.Body)
		dec.Decode(blk.Header)
		resp.Body.Close()
	} else {
		blk.Header.ParentId = hex.EncodeToString(parent)
		blk.Header.Difficulty = uint64(*genesisDifficulty)
	}
	blk.Header.Timestamp = uint64(time.Now().Add(2 * time.Minute).UnixNano())
	blk.setRoot()

	log.Println("parent ID", blk.Header.ParentId)

	col := newCollider(blk.Header)
	blk.Header.Nonces = col.collide()

	encblk, err := json.Marshal(blk)
	if err != nil {
		log.Println(err)
		return nil
	}

	resp, err := client.Post("http://6857coin.csail.mit.edu:8080/add", "application/json", bytes.NewBuffer(encblk))
	if err != nil {
		log.Println(err)
		return nil
	}
	err = printResponse(resp)
	if err != nil {
		return nil
	}

	fullHash := blk.Header.fullHash()
	log.Println("block committed", hex.EncodeToString(fullHash))
	return fullHash
}

func printResponse(resp *http.Response) error {
	defer resp.Body.Close()
	contents, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	log.Println("response", string(contents))
	return nil
}

func mask(difficulty uint64) uint64 {
	return (1 << difficulty) - 1
}

type BlockHeader struct {
	ParentId   string   `json:'parentid'`
	Root       string   `json:'root'`
	Difficulty uint64   `json:'difficulty'`
	Timestamp  uint64   `json:'timestamp'`
	Nonces     []uint64 `json:'nonces'`
	Version    byte     `json:'byte'`
}

type Block struct {
	Header *BlockHeader `json:'header'`
	Block  string       `json:'block'`
}

func (b *Block) setRoot() {
	sum := sha256.Sum256([]byte(b.Block))
	b.Header.Root = hex.EncodeToString(sum[:])
}

func (b *BlockHeader) fullHash() []byte {
	h := sha256.New()
	parentId, _ := hex.DecodeString(b.ParentId)
	h.Write(parentId)
	root, _ := hex.DecodeString(b.Root)
	h.Write(root)
	binary.Write(h, binary.BigEndian, b.Difficulty)
	binary.Write(h, binary.BigEndian, b.Timestamp)
	for _, v := range b.Nonces {
		binary.Write(h, binary.BigEndian, v)
	}
	h.Write([]byte{b.Version})
	return h.Sum(nil)
}

func (b *BlockHeader) getHashBytes() []byte {
	buf := &bytes.Buffer{}
	parentId, _ := hex.DecodeString(b.ParentId)
	buf.Write(parentId)
	root, _ := hex.DecodeString(b.Root)
	buf.Write(root)
	binary.Write(buf, binary.BigEndian, b.Difficulty)
	binary.Write(buf, binary.BigEndian, b.Timestamp)
	binary.Write(buf, binary.BigEndian, uint64(0)) // nonce
	buf.WriteByte(b.Version)

	return buf.Bytes()
}

type Entry struct {
	nonceA uint64
	nonceB uint64
	sum    uint64
}

type Collider struct {
	tableMask uint64
	entries   []C.struct_entry
	locks     []C.mutex
	header    *BlockHeader
}

func newCollider(h *BlockHeader) *Collider {
	size := (1 << (h.Difficulty * 2 / 3))
	maxSize := 1 << uint64(*maxTable)
	if size > maxSize {
		size = maxSize
	}
	log.Println("table size", size)
	return &Collider{
		tableMask: uint64(size - 1),
		entries:   make([]C.struct_entry, size),
		locks:     make([]C.mutex, 65536),
		header:    h,
	}
}

func (c *Collider) collideWorker(res chan []uint64, stop chan bool, progress chan uint64, wg *sync.WaitGroup) {
	defer wg.Done()
	iters := 10000
	header := c.header.getHashBytes()
	headerCopy := make([]C.uint8_t, len(header))
	for i := range header {
		headerCopy[i] = C.uint8_t(header[i])
	}
	result := make([]C.uint64_t, 3)

	for {
		found := C.find_collisions(&c.entries[0], &c.locks[0],
			C.uint64_t(c.tableMask),
			C.uint64_t(mask(c.header.Difficulty)),
			C.uint64_t(rand.Int63()),
			&headerCopy[0],
			C.int(iters),
			&result[0])

		if found != 0 {
			nonces := make([]uint64, 3)
			for i := range result {
				nonces[i] = uint64(result[i])
			}
			select {
			case res <- nonces:
			case <-stop:
			}
			return
		}

		select {
		case progress <- uint64(iters):
		case <-stop:
			return
		}
		select {
		case <-stop:
			return
		default:
		}
	}
}

func (c *Collider) collide() (nonces []uint64) {
	workers := runtime.NumCPU() * 2

	res := make(chan []uint64)
	progress := make(chan uint64)
	wg := sync.WaitGroup{}
	defer wg.Wait()
	stop := make(chan bool)
	defer close(stop)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go c.collideWorker(res, stop, progress, &wg)
	}

	count := uint64(0)
	now := time.Now()
	start := now
	for {
		select {
		case nonces = <-res:
			log.Println("found nonces", nonces)
			log.Printf("%d nonces, %.6f seconds\n", count, time.Now().Sub(start).Seconds())
			return
		case <-stop:
			return nil
		case incr := <-progress:
			count += incr
			mod := uint64(100000000)
			if count%mod == 0 {
				prev := now
				now = time.Now()
				delta := now.Sub(prev)
				log.Printf("%d nonces, %.6f million hashes/sec",
					count, float64(mod) / 1000000 / delta.Seconds())
			}
		}
	}
}
