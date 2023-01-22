//go:build debugcharts
// +build debugcharts

package main

import (
	_ "net/http/pprof"

	_ "github.com/mkevac/debugcharts"
)
