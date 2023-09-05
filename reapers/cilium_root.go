package reapers

import (
	"os"
	"path/filepath"

	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
)

var binaryName = filepath.Base(os.Args[0])

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, binaryName)
