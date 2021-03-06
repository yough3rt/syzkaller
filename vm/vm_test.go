// Copyright 2018 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package vm

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/report"
	"github.com/google/syzkaller/vm/vmimpl"
)

type testPool struct {
}

func (pool *testPool) Count() int {
	return 1
}

func (pool *testPool) Create(workdir string, index int) (vmimpl.Instance, error) {
	return &testInstance{
		outc: make(chan []byte, 10),
		errc: make(chan error, 1),
	}, nil
}

type testInstance struct {
	outc        chan []byte
	errc        chan error
	diagnoseBug bool
}

func (inst *testInstance) Copy(hostSrc string) (string, error) {
	return "", nil
}

func (inst *testInstance) Forward(port int) (string, error) {
	return "", nil
}

func (inst *testInstance) Run(timeout time.Duration, stop <-chan bool, command string) (
	outc <-chan []byte, errc <-chan error, err error) {
	return inst.outc, inst.errc, nil
}

func (inst *testInstance) Diagnose() bool {
	if inst.diagnoseBug {
		inst.outc <- []byte("BUG: DIAGNOSE\n")
	} else {
		inst.outc <- []byte("DIAGNOSE\n")
	}
	return true
}

func (inst *testInstance) Close() {
}

func init() {
	beforeContext = 200
	tickerPeriod = 1 * time.Second
	noOutputTimeout = 5 * time.Second
	waitForOutputTimeout = 3 * time.Second

	ctor := func(env *vmimpl.Env) (vmimpl.Pool, error) {
		return &testPool{}, nil
	}
	vmimpl.Register("test", ctor, false)
}

type Test struct {
	Name        string
	CanExit     bool // if the program is allowed to exit normally
	DiagnoseBug bool // Diagnose produces output that is detected as kernel crash
	Body        func(outc chan []byte, errc chan error)
	Report      *report.Report
}

var tests = []*Test{
	{
		Name:    "program-exits-normally",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			time.Sleep(time.Second)
			errc <- nil
		},
	},
	{
		Name: "program-exits-when-it-should-not",
		Body: func(outc chan []byte, errc chan error) {
			time.Sleep(time.Second)
			errc <- nil
		},
		Report: &report.Report{
			Title: lostConnectionCrash,
		},
	},
	{
		Name:        "#875-diagnose-bugs",
		CanExit:     true,
		DiagnoseBug: true,
		Body: func(outc chan []byte, errc chan error) {
			errc <- nil
		},
	},
	{
		Name: "#875-diagnose-bugs-2",
		Body: func(outc chan []byte, errc chan error) {
			errc <- nil
		},
		Report: &report.Report{
			Title: lostConnectionCrash,
			Output: []byte(
				"DIAGNOSE\n",
			),
		},
	},
	{
		Name: "kernel-crashes",
		Body: func(outc chan []byte, errc chan error) {
			outc <- []byte("BUG: bad\n")
			time.Sleep(time.Second)
			outc <- []byte("other output\n")
		},
		Report: &report.Report{
			Title: "BUG: bad",
			Report: []byte(
				"BUG: bad\n" +
					"DIAGNOSE\n" +
					"other output\n",
			),
		},
	},
	{
		Name: "fuzzer-is-preempted",
		Body: func(outc chan []byte, errc chan error) {
			outc <- []byte("BUG: bad\n")
			outc <- []byte(fuzzerPreemptedStr + "\n")
		},
	},
	{
		Name:    "program-exits-but-kernel-crashes-afterwards",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			errc <- nil
			time.Sleep(time.Second)
			outc <- []byte("BUG: bad\n")
		},
		Report: &report.Report{
			Title: "BUG: bad",
			Report: []byte(
				"BUG: bad\n" +
					"DIAGNOSE\n",
			),
		},
	},
	{
		Name: "timeout",
		Body: func(outc chan []byte, errc chan error) {
			errc <- vmimpl.ErrTimeout
		},
	},
	{
		Name: "program-crashes",
		Body: func(outc chan []byte, errc chan error) {
			errc <- fmt.Errorf("error")
		},
		Report: &report.Report{
			Title: lostConnectionCrash,
		},
	},
	{
		Name: "no-output-1",
		Body: func(outc chan []byte, errc chan error) {
		},
		Report: &report.Report{
			Title: noOutputCrash,
		},
	},
	{
		Name: "no-output-2",
		Body: func(outc chan []byte, errc chan error) {
			for i := 0; i < 5; i++ {
				time.Sleep(time.Second)
				outc <- []byte("something\n")
			}
		},
		Report: &report.Report{
			Title: noOutputCrash,
		},
	},
	{
		Name:    "no-no-output-1",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			for i := 0; i < 5; i++ {
				time.Sleep(time.Second)
				outc <- []byte(executingProgramStr1 + "\n")
			}
			errc <- nil
		},
	},
	{
		Name:    "no-no-output-2",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			for i := 0; i < 5; i++ {
				time.Sleep(time.Second)
				outc <- []byte(executingProgramStr2 + "\n")
			}
			errc <- nil
		},
	},
	{
		Name:    "outc-closed",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			close(outc)
			time.Sleep(time.Second)
			errc <- vmimpl.ErrTimeout
		},
	},
	{
		Name:    "lots-of-output",
		CanExit: true,
		Body: func(outc chan []byte, errc chan error) {
			for i := 0; i < 100; i++ {
				outc <- []byte("something\n")
			}
			time.Sleep(time.Second)
			errc <- vmimpl.ErrTimeout
		},
	},
}

func TestMonitorExecution(t *testing.T) {
	for _, test := range tests {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			testMonitorExecution(t, test)
		})
	}
}

func testMonitorExecution(t *testing.T, test *Test) {
	dir, err := ioutil.TempDir("", "syz-vm-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := &mgrconfig.Config{
		Workdir:      dir,
		TargetOS:     "linux",
		TargetArch:   "amd64",
		TargetVMArch: "amd64",
		Type:         "test",
	}
	pool, err := Create(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	reporter, err := report.NewReporter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := pool.Create(0)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close()
	outc, errc, err := inst.Run(time.Second, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	testInst := inst.impl.(*testInstance)
	testInst.diagnoseBug = test.DiagnoseBug
	done := make(chan bool)
	go func() {
		test.Body(testInst.outc, testInst.errc)
		done <- true
	}()
	rep := inst.MonitorExecution(outc, errc, reporter, test.CanExit)
	<-done
	if test.Report != nil && rep == nil {
		t.Fatalf("got no report")
	}
	if test.Report == nil && rep != nil {
		t.Fatalf("got unexpected report: %v", rep.Title)
	}
	if test.Report == nil {
		return
	}
	if test.Report.Title != rep.Title {
		t.Fatalf("want title %q, got title %q", test.Report.Title, rep.Title)
	}
	if !bytes.Equal(test.Report.Report, rep.Report) {
		t.Fatalf("want report:\n%s\n\ngot report:\n%s\n", test.Report.Report, rep.Report)
	}
	if test.Report.Output != nil && !bytes.Equal(test.Report.Output, rep.Output) {
		t.Fatalf("want output:\n%s\n\ngot output:\n%s\n", test.Report.Output, rep.Output)
	}
}
