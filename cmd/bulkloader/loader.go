package main

import (
	"bufio"
	"compress/gzip"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/x"
)

type options struct {
	rdfFile       string
	schemaFile    string
	badgerDir     string
	tmpDir        string
	numGoroutines int
}

type state struct {
	opt        options
	prog       *progress
	um         *uidMap
	ss         *schemaStore
	rdfCh      chan string
	postingsCh chan *protos.FlatPosting
}

type loader struct {
	*state
	mappers   []*mapper
	mapOutput []string
}

func newLoader(opt options) *loader {
	schemaBuf, err := ioutil.ReadFile(opt.schemaFile)
	x.Checkf(err, "Could not load schema.")
	initialSchema, err := schema.Parse(string(schemaBuf))
	x.Checkf(err, "Could not parse schema.")

	st := &state{
		opt:        opt,
		prog:       newProgress(),
		um:         newUIDMap(),
		ss:         newSchemaStore(initialSchema),
		rdfCh:      make(chan string, 1000),
		postingsCh: make(chan *protos.FlatPosting, 1000),
	}
	ld := &loader{
		state:   st,
		mappers: make([]*mapper, opt.numGoroutines),
	}
	for i := 0; i < opt.numGoroutines; i++ {
		ld.mappers[i] = &mapper{state: st}
	}
	return ld
}

func (ld *loader) mapStage() {
	go ld.prog.report()

	var postingWriterWg sync.WaitGroup
	postingWriterWg.Add(1)

	tmpPostingsDir, err := ioutil.TempDir(ld.opt.tmpDir, "bulkloader_tmp_posting_")
	x.Check(err)

	go func() {
		ld.mapOutput = writeMapOutput(tmpPostingsDir, ld.postingsCh, ld.prog)
		postingWriterWg.Done()
	}()

	f, err := os.Open(ld.opt.rdfFile)
	x.Checkf(err, "Could not read RDF file.")
	defer f.Close()

	var mapperWg sync.WaitGroup
	mapperWg.Add(len(ld.mappers))
	for _, m := range ld.mappers {
		go func(m *mapper) {
			m.run()
			mapperWg.Done()
		}(m)
	}

	var sc *bufio.Scanner
	if !strings.HasSuffix(ld.opt.rdfFile, ".gz") {
		sc = bufio.NewScanner(f)
	} else {
		gzr, err := gzip.NewReader(f)
		x.Checkf(err, "Could not create gzip reader for RDF file.")
		sc = bufio.NewScanner(gzr)
	}

	for i := 0; sc.Scan(); i++ {
		ld.rdfCh <- sc.Text()
	}
	x.Check(sc.Err())

	close(ld.rdfCh)
	mapperWg.Wait()
	close(ld.postingsCh)
	postingWriterWg.Wait()
}

func (ld *loader) reduceStage() {
	// Read from map stage.
	shuffleInputChs := make([]chan *protos.FlatPosting, len(ld.mapOutput))
	for i, mappedFile := range ld.mapOutput {
		shuffleInputChs[i] = make(chan *protos.FlatPosting, 1000)
		go readMapOutput(mappedFile, shuffleInputChs[i])
	}

	// Shuffle concurrently with reduce.
	reduceCh := make(chan []*protos.FlatPosting, 3) // Small buffer size since each element has a lot of data.
	go shufflePostings(reduceCh, shuffleInputChs, ld.prog)

	// Reduce stage.
	pending := make(chan struct{}, ld.opt.numGoroutines)
	var reduceWg sync.WaitGroup
	for batch := range reduceCh {
		pending <- struct{}{}
		reduceWg.Add(1)
		go func() {
			reduce(batch, ld.prog)
			<-pending
			reduceWg.Done()
		}()
	}
	reduceWg.Wait()

	ld.prog.endSummary()
}

func (ld *loader) cleanup() {
	if len(ld.mapOutput) > 0 {
		dir := filepath.Dir(ld.mapOutput[0])
		x.Check(os.RemoveAll(dir))
	}
}