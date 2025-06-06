// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
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

package testcases

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/apache/skywalking-banyandb/pkg/index"
	"github.com/apache/skywalking-banyandb/pkg/index/posting"
	"github.com/apache/skywalking-banyandb/pkg/index/posting/roaring"
)

var serviceName = index.FieldKey{
	SeriesID: 887,
	// http_method
	IndexRuleID: 6,
}

// RunServiceName executes service name related cases.
func RunServiceName(t *testing.T, store SimpleStore) {
	tester := assert.New(t)
	tests := []struct {
		want    posting.List
		name    string
		arg     index.Field
		wantErr bool
	}{
		{
			name: "match gateway",
			arg:  index.NewStringField(serviceName, "gateway"),
			want: roaring.NewRange(0, 50),
		},
		{
			name: "match webpage",
			arg:  index.NewStringField(serviceName, "webpage"),
			want: roaring.NewRange(50, 100),
		},
		{
			name: "unknown field",
			want: roaring.DummyPostingList,
		},
		{
			name: "unknown term",
			arg:  index.NewStringField(serviceName, "unknown"),
			want: roaring.DummyPostingList,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(_ *testing.T) {
			list, _, err := store.MatchTerms(tt.arg)
			if tt.wantErr {
				tester.Error(err)
				return
			}
			tester.NoError(err)
			tester.NotNil(list)
			tester.True(tt.want.Equal(list))
		})
	}
}

// SetUp initializes a index repository.
func SetUp(t *assert.Assertions, store SimpleStore) {
	var batch index.Batch
	for i := 0; i < 100; i++ {
		if i < 100/2 {
			batch.Documents = append(batch.Documents, index.Document{
				Fields: []index.Field{
					index.NewStringField(serviceName, "gateway"),
				},
				DocID: uint64(i),
			})
		} else {
			batch.Documents = append(batch.Documents, index.Document{
				Fields: []index.Field{
					index.NewStringField(serviceName, "webpage"),
				},
				DocID: uint64(i),
			})
		}
	}
	t.NoError(store.Batch(batch))
}
