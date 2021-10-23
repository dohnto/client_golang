// Copyright 2022 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	//nolint:staticcheck // Ignore SA1019. Need to keep deprecated package for compatibility.
	"github.com/golang/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus/internal"
	dto "github.com/prometheus/client_model/go"
)

var _ prometheus.TransactionalGatherer = &CachedTGatherer{}

var separatorByteSlice = []byte{model.SeparatorByte} // For convenient use with xxhash.

// CachedTGatherer is a transactional gatherer that allows maintaining a set of metrics which
// change less frequently than scrape time, yet label values and values change over time.
//
// If you happen to use NewDesc, NewConstMetric or MustNewConstMetric inside Collector.Collect routine, consider
// using CachedTGatherer instead.
//
// Use CachedTGatherer with classic Registry using NewMultiTRegistry and ToTransactionalGatherer helpers.
// NOTE(bwplotka): Experimental, API and behaviour can change.
type CachedTGatherer struct {
	metrics            map[uint64]*dto.Metric
	metricFamilyByName map[string]*dto.MetricFamily
	mMu                sync.RWMutex
}

func NewCachedTGatherer() *CachedTGatherer {
	return &CachedTGatherer{
		metrics:            make(map[uint64]*dto.Metric),
		metricFamilyByName: map[string]*dto.MetricFamily{},
	}
}

// Gather implements TransactionalGatherer interface.
func (c *CachedTGatherer) Gather() (_ []*dto.MetricFamily, done func(), err error) {
	c.mMu.RLock()

	// BenchmarkCachedTGatherer_Update shows, even for 1 million metrics with 1000 families
	// this is efficient enough (~300µs and ~50 kB per op), no need to cache it for now.
	return internal.NormalizeMetricFamilies(c.metricFamilyByName), c.mMu.RUnlock, nil
}

type Key struct {
	FQName string // __name__

	// Label names can be unsorted, we will be sorting them later. The only implication is cachability if
	// consumer provide non-deterministic order of those.
	LabelNames  []string
	LabelValues []string
}

func (k Key) isValid() error {
	if k.FQName == "" {
		return errors.New("FQName cannot be empty")
	}
	if len(k.LabelNames) != len(k.LabelValues) {
		return errors.New("new metric: label name has different length than values")
	}

	return nil
}

// hash returns unique hash for this key.
func (k Key) hash() uint64 {
	h := xxhash.New()
	h.WriteString(k.FQName)
	h.Write(separatorByteSlice)

	for i := range k.LabelNames {
		h.WriteString(k.LabelNames[i])
		h.Write(separatorByteSlice)
		h.WriteString(k.LabelValues[i])
		h.Write(separatorByteSlice)
	}
	return h.Sum64()
}

// Insert represents record to set in cache.
type Insert struct {
	Key

	Help      string
	ValueType prometheus.ValueType
	Value     float64

	// Timestamp is optional. Pass nil for no explicit timestamp.
	Timestamp *time.Time
}

// Update goes through inserts and deletions and updates current cache in concurrency safe manner.
// If reset is set to true, all inserts and deletions are working on empty cache. In such case
// this implementation tries to reuse memory from existing cached item when possible.
//
// Update reuses insert struct memory, so after use, Insert slice and its elements cannot be reused
// outside of this method.
// TODO(bwplotka): Lack of copying can pose memory safety problems if insert variables are reused. Consider copying if value
// is different. Yet it gives significant allocation gains.
func (c *CachedTGatherer) Update(reset bool, inserts []Insert, deletions []Key) error {
	c.mMu.Lock()
	defer c.mMu.Unlock()

	currMetrics := c.metrics
	currMetricFamilies := c.metricFamilyByName
	if reset {
		currMetrics = make(map[uint64]*dto.Metric, len(c.metrics))
		currMetricFamilies = make(map[string]*dto.MetricFamily, len(c.metricFamilyByName))
	}

	errs := prometheus.MultiError{}
	for i := range inserts {
		// TODO(bwplotka): Validate more about this insert?
		if err := inserts[i].isValid(); err != nil {
			errs.Append(err)
			continue
		}

		// Update metric family.
		mf, ok := c.metricFamilyByName[inserts[i].FQName]
		if !ok {
			mf = &dto.MetricFamily{}
			mf.Name = &inserts[i].FQName
		} else if reset {
			// Reset metric slice, since we want to start from scratch.
			mf.Metric = mf.Metric[:0]
		}
		mf.Type = inserts[i].ValueType.ToDTO()
		mf.Help = &inserts[i].Help

		currMetricFamilies[inserts[i].FQName] = mf

		// Update metric pointer.
		hSum := inserts[i].hash()
		m, ok := c.metrics[hSum]
		if !ok {
			m = &dto.Metric{Label: make([]*dto.LabelPair, 0, len(inserts[i].LabelNames))}
			for j := range inserts[i].LabelNames {
				m.Label = append(m.Label, &dto.LabelPair{
					Name:  &inserts[i].LabelNames[j],
					Value: &inserts[i].LabelValues[j],
				})
			}
			sort.Sort(internal.LabelPairSorter(m.Label))
		}

		switch inserts[i].ValueType {
		case prometheus.CounterValue:
			v := m.Counter
			if v == nil {
				v = &dto.Counter{}
			}
			v.Value = &inserts[i].Value
			m.Counter = v
			m.Gauge = nil
			m.Untyped = nil
		case prometheus.GaugeValue:
			v := m.Gauge
			if v == nil {
				v = &dto.Gauge{}
			}
			v.Value = &inserts[i].Value
			m.Counter = nil
			m.Gauge = v
			m.Untyped = nil
		case prometheus.UntypedValue:
			v := m.Untyped
			if v == nil {
				v = &dto.Untyped{}
			}
			v.Value = &inserts[i].Value
			m.Counter = nil
			m.Gauge = nil
			m.Untyped = v
		default:
			return fmt.Errorf("unsupported value type %v", inserts[i].ValueType)
		}

		m.TimestampMs = nil
		if inserts[i].Timestamp != nil {
			m.TimestampMs = proto.Int64(inserts[i].Timestamp.Unix()*1000 + int64(inserts[i].Timestamp.Nanosecond()/1000000))
		}
		currMetrics[hSum] = m

		if !reset && ok {
			// If we did update without reset and we found metric in previous
			// map, we know metric pointer exists in metric family map, so just continue.
			continue
		}

		// Will be sorted later anyway, so just append.
		mf.Metric = append(mf.Metric, m)
	}

	for _, del := range deletions {
		if err := del.isValid(); err != nil {
			errs.Append(err)
			continue
		}

		hSum := del.hash()
		m, ok := currMetrics[hSum]
		if !ok {
			continue
		}
		delete(currMetrics, hSum)

		mf, ok := currMetricFamilies[del.FQName]
		if !ok {
			// Impossible, but well...
			errs.Append(fmt.Errorf("could not remove metric %s(%s) from metric family, metric family does not exists", del.FQName, del.LabelValues))
			continue
		}

		toDel := -1
		for i := range mf.Metric {
			if mf.Metric[i] == m {
				toDel = i
				break
			}
		}

		if toDel == -1 {
			errs.Append(fmt.Errorf("could not remove metric %s(%s) from metric family, metric family does not have such metric", del.FQName, del.LabelValues))
			continue
		}

		if len(mf.Metric) == 1 {
			delete(currMetricFamilies, del.FQName)
			continue
		}

		mf.Metric = append(mf.Metric[:toDel], mf.Metric[toDel+1:]...)
	}

	c.metrics = currMetrics
	c.metricFamilyByName = currMetricFamilies
	return errs.MaybeUnwrap()
}
