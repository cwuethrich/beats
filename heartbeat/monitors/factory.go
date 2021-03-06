// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package monitors

import (
	"fmt"

	"github.com/elastic/beats/v7/heartbeat/scheduler"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/cfgfile"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/fmtstr"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/processors"
	"github.com/elastic/beats/v7/libbeat/processors/add_formatted_index"
	"github.com/elastic/beats/v7/libbeat/publisher/pipetool"
)

// RunnerFactory that can be used to create cfg.Runner cast versions of Monitor
// suitable for config reloading.
type RunnerFactory struct {
	info         beat.Info
	sched        *scheduler.Scheduler
	allowWatches bool
}

type publishSettings struct {
	// Fields and tags to add to monitor.
	EventMetadata common.EventMetadata    `config:",inline"`
	Processors    processors.PluginConfig `config:"processors"`

	PublisherPipeline struct {
		DisableHost bool `config:"disable_host"` // Disable addition of host.name.
	} `config:"publisher_pipeline"`

	// KeepNull determines whether published events will keep null values or omit them.
	KeepNull bool `config:"keep_null"`

	// Output meta data settings
	Pipeline   string                   `config:"pipeline"` // ES Ingest pipeline name
	Index      fmtstr.EventFormatString `config:"index"`    // ES output index pattern
	DataStream *datastream              `config:"data_stream"`
	DataSet    string                   `config:"dataset"`
}

type datastream struct {
	Namespace string `config:"namespace"`
	Dataset   string `config:"dataset"`
	Type      string `config:"type"`
}

// NewFactory takes a scheduler and creates a RunnerFactory that can create cfgfile.Runner(Monitor) objects.
func NewFactory(info beat.Info, sched *scheduler.Scheduler, allowWatches bool) *RunnerFactory {
	return &RunnerFactory{info, sched, allowWatches}
}

// Create makes a new Runner for a new monitor with the given Config.
func (f *RunnerFactory) Create(p beat.Pipeline, c *common.Config) (cfgfile.Runner, error) {
	configEditor, err := newCommonPublishConfigs(f.info, c)
	if err != nil {
		return nil, err
	}

	p = pipetool.WithClientConfigEdit(p, configEditor)
	monitor, err := newMonitor(c, globalPluginsReg, p, f.sched, f.allowWatches)
	return monitor, err
}

// CheckConfig checks to see if the given monitor config is valid.
func (f *RunnerFactory) CheckConfig(config *common.Config) error {
	return checkMonitorConfig(config, globalPluginsReg, f.allowWatches)
}

func newCommonPublishConfigs(info beat.Info, cfg *common.Config) (pipetool.ConfigEditor, error) {
	var settings publishSettings
	if err := cfg.Unpack(&settings); err != nil {
		return nil, err
	}

	indexProcessor, err := setupIndexProcessor(info, settings)
	if err != nil {
		return nil, err
	}

	userProcessors, err := processors.New(settings.Processors)
	if err != nil {
		return nil, err
	}

	// TODO: Remove this logic in the 8.0/master branch, preserve only in 7.x
	dataset := settings.DataSet
	if dataset == "" {
		if settings.DataStream != nil && settings.DataStream.Dataset != "" {
			dataset = settings.DataStream.Dataset
		} else {
			dataset = "uptime"
		}
	}

	return func(clientCfg beat.ClientConfig) (beat.ClientConfig, error) {
		logp.Info("Client connection with: %#v", clientCfg)

		fields := clientCfg.Processing.Fields.Clone()
		fields.Put("event.dataset", dataset)

		meta := clientCfg.Processing.Meta.Clone()
		if settings.Pipeline != "" {
			meta.Put("pipeline", settings.Pipeline)
		}

		// assemble the processors. Ordering is important.
		// 1. add support for index configuration via processor
		// 2. add processors added by the input that wants to connect
		// 3. add locally configured processors from the 'processors' settings
		procs := processors.NewList(nil)
		if indexProcessor != nil {
			procs.AddProcessor(indexProcessor)
		}
		if lst := clientCfg.Processing.Processor; lst != nil {
			procs.AddProcessor(lst)
		}
		if userProcessors != nil {
			procs.AddProcessors(*userProcessors)
		}

		clientCfg.Processing.EventMetadata = settings.EventMetadata
		clientCfg.Processing.Fields = fields
		clientCfg.Processing.Meta = meta
		clientCfg.Processing.Processor = procs
		clientCfg.Processing.KeepNull = settings.KeepNull
		clientCfg.Processing.DisableHost = settings.PublisherPipeline.DisableHost

		return clientCfg, nil
	}, nil
}

func setupIndexProcessor(info beat.Info, settings publishSettings) (processors.Processor, error) {
	var indexProcessor processors.Processor
	if settings.DataStream != nil {
		namespace := settings.DataStream.Namespace
		if namespace == "" {
			namespace = "default"
		}
		typ := settings.DataStream.Type
		if typ == "" {
			typ = "synthetics"
		}

		dataset := settings.DataStream.Dataset
		if dataset == "" {
			dataset = "generic"
		}

		index := fmt.Sprintf(
			"%s-%s-%s",
			typ,
			dataset,
			namespace,
		)
		compiled, err := fmtstr.CompileEvent(index)
		if err != nil {
			return nil, fmt.Errorf("could not compile datastream: '%s', this should never happen: %w", index, err)
		} else {
			settings.Index = *compiled
		}
	}

	if !settings.Index.IsEmpty() {
		staticFields := fmtstr.FieldsForBeat(info.Beat, info.Version)

		timestampFormat, err :=
			fmtstr.NewTimestampFormatString(&settings.Index, staticFields)
		if err != nil {
			return nil, err
		}
		indexProcessor = add_formatted_index.New(timestampFormat)
	}
	return indexProcessor, nil
}
