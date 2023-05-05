package oncpu

import (
	"context"
	"fmt"
	"perfprofiler/pkg/process/api"
	"perfprofiler/pkg/process/finders"
	"perfprofiler/pkg/process/finders/scanner"
	"perfprofiler/pkg/profiling/task/base"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLoad(t *testing.T) {
	conf := &base.TaskConfig{
		OnCPU: &base.OnCPUConfig{
			Period: "10ms",
		},
	}
	r, e := NewRunner(conf, nil)
	assert.NoError(t, e)

	task := &base.ProfilingTask{
		TaskID:          "clickhouse-server",
		ProcessIDList:   []string{"494301"},
		UpdateTime:      time.Now().Unix(),
		StartTime:       time.Now().Unix(),
		TargetType:      base.TargetTypeOnCPU,
		TriggerType:     "",
		ExtensionConfig: &base.ExtensionConfig{},
	}
	assert.NoError(t, e)

	finder := &scanner.ProcessFinder{}
	finderConf := &scanner.Config{
		Period:   "30s",
		ScanMode: scanner.Regex,
		Agent:    nil,
		RegexFinders: []*scanner.RegexFinder{{
			MatchCommandRegex: ".*/usr/bin/clickhouse-server.*",
			Layer:             "OS_LINUX",
			ServiceName:       "clickhouse-server",
			InstanceName:      "{{.Rover.HostIPV4 \"p3p2\"}}",
			ProcessName:       "clickhouse-server",
		}},
	}
	finder.Init(context.Background(), finderConf, nil)
	pes, e := finder.FindProcesses()
	assert.NoError(t, e)

	processes := make([]api.ProcessInterface, 0)
	for _, pesi := range pes {
		p := finders.NewProcessContext(finder.DetectType(), pesi)
		processes = append(processes, p)
	}

	if e = r.Init(task, processes); e != nil {
		log.Fatal(fmt.Sprintf("error calling Run - %v", e))
	}
	go func() {
		notify := func() {}
		e = r.Run(context.Background(), notify)
		if e != nil {
			log.Fatal(fmt.Sprintf("error calling Run - %v", e))
		}
	}()

	for {
		data, e := r.FlushData()
		assert.NoError(t, e)

		if len(data) == 0 {
			continue
		}
	}
}
