// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package sql

import (
	"testing"

	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/util/leaktest"
)

func TestNormalizeName(t *testing.T) {
	defer leaktest.AfterTest(t)
	testCases := []struct {
		in, expected string
	}{
		{"HELLO", "hello"},                            // Lowercase is the norm
		{"ıİ", "ii"},                                  // Turkish/Azeri special cases
		{"no\u0308rmalization", "n\u00f6rmalization"}, // NFD -> NFC.
	}
	for _, test := range testCases {
		s := normalizeName(test.in)
		if test.expected != s {
			t.Errorf("%s: expected %s, but found %s", test.in, test.expected, s)
		}
	}
}

func TestKeyAddress(t *testing.T) {
	defer leaktest.AfterTest(t)
	testCases := []struct {
		key, expAddress proto.Key
	}{
		{MakeNameMetadataKey(0, "foo"), proto.Key("\xff\n\x02\n\x01\tfoo\x00\x01\n\x03")},
		{MakeNameMetadataKey(0, "BAR"), proto.Key("\xff\n\x02\n\x01\tbar\x00\x01\n\x03")},
		{MakeDescMetadataKey(123), proto.Key("\xff\n\x03\n\x01\n{\n\x02")},
	}
	for i, test := range testCases {
		result := keys.KeyAddress(test.key)
		if !result.Equal(test.expAddress) {
			t.Errorf("%d: expected address for key %q doesn't match %q", i, test.key, test.expAddress)
		}
	}
}
