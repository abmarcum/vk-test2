// is used instead so the module remains buildable on Go 1.19+.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// healthCheckerAdapter satisfies the healthChecker interface (declared in
