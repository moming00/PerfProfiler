package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"perfprofiler/pkg/process/api"
	"perfprofiler/pkg/process/finders"
	"perfprofiler/pkg/process/finders/scanner"
	"perfprofiler/pkg/profiling/task/base"
	"perfprofiler/pkg/profiling/task/callchain/test"
	"strconv"
	"time"
)

func getProcessName(pid int) (string, error) {
	// Convert the PID to a string
	pidStr := strconv.Itoa(pid)

	// Open the process's comm file
	commFile, err := os.Open("/proc/" + pidStr + "/comm")
	if err != nil {
		return "", err
	}
	defer commFile.Close()

	// Read the contents of the file
	commBytes, err := ioutil.ReadAll(commFile)
	if err != nil {
		return "", err
	}

	// Convert the bytes to a string and remove any trailing newline characters
	commStr := string(commBytes[:len(commBytes)-1])

	return commStr, nil
}
func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {

		fmt.Printf("must have pid specified")
	}
	pid, e := strconv.Atoi(args[0])
	if e != nil {
		log.Fatalf("unable to find process %v", e)
	}

	name, err := getProcessName(pid)
	if err != nil {
		log.Fatalf("Error getting process name:%v", err)
	}

	conf := &base.TaskConfig{
		OnCPU: &base.OnCPUConfig{
			Period: "10ms",
		},
	}
	r, _ := test.NewRunner(conf, nil)

	task := &base.ProfilingTask{
		TaskID:          name,
		ProcessIDList:   []string{fmt.Sprintf("%d", pid)},
		UpdateTime:      time.Now().Unix(),
		StartTime:       time.Now().Unix(),
		TargetType:      base.TargetTypeOnCPU,
		TriggerType:     "",
		ExtensionConfig: &base.ExtensionConfig{},
	}

	finder := &scanner.ProcessFinder{}
	finderConf := &scanner.Config{
		Period:   "30s",
		ScanMode: scanner.Regex,
		Agent:    nil,
		RegexFinders: []*scanner.RegexFinder{{
			MatchCommandRegex: name,
			Layer:             "OS_LINUX",
			ServiceName:       name,
			InstanceName:      "{{.Rover.HostIPV4 \"p3p2\"}}",
			ProcessName:       name,
		}},
	}
	finder.Init(context.Background(), finderConf, nil)
	pes, _ := finder.FindProcesses()

	processes := make([]api.ProcessInterface, 0)
	for _, pesi := range pes {
		p := finders.NewProcessContext(finder.DetectType(), pesi)
		processes = append(processes, p)
	}

	if e := r.Init(task, processes); e != nil {
		log.Fatal(fmt.Sprintf("error calling Run - %v", e))
	}

	e = r.Run(context.Background(), func() {})
	if e != nil {
		log.Fatal(fmt.Sprintf("error calling Run - %v", e))
	}

	for {
		data, _ := r.FlushData()

		if len(data) == 0 {
			continue
		}
	}
}
