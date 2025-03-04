// Copyright 2017-2019 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vpd

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetReadOnly(t *testing.T) {
	r := NewReader()
	r.VpdDir = "./tests"
	value, err := r.Get("key1", true)
	require.NoError(t, err)
	assert.Equal(t, value, []byte("value1\n"))
}

func TestGetReadWrite(t *testing.T) {
	r := NewReader()
	r.VpdDir = "./tests"
	value, err := r.Get("mysecretpassword", false)
	require.NoError(t, err)
	assert.Equal(t, value, []byte("passw0rd\n"))
}

func TestGetReadBinary(t *testing.T) {
	r := NewReader()
	r.VpdDir = "./tests"
	value, err := r.Get("binary1", true)
	require.NoError(t, err)
	assert.Equal(t, value, []byte("some\x00binary\ndata"))
}

func TestGetAllReadOnly(t *testing.T) {
	r := NewReader()
	r.VpdDir = "./tests"
	expected := map[string][]byte{
		"binary1": []byte("some\x00binary\ndata"),
		"key1":    []byte("value1\n"),
	}
	vpdMap, err := r.GetAll(true)
	require.NoError(t, err)
	if !reflect.DeepEqual(vpdMap, expected) {
		t.FailNow()
	}
}
