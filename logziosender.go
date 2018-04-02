// Copyright © 2017 Douglas Chimento <dchimento@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logzio

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/beeker1121/goque"
	"go.uber.org/atomic"
	"github.com/ricochet2200/go-disk-usage/du"
)

const (
	maxSize              	= 3 * 1024 * 1024 // 3 mb
	sendSleepingBackoff     = time.Second * 2
	sendRetries             = 4
	respReadLimit 			= int64(4096)

	defaultHost          	= "https://listener.logz.io:8071"
	defaultDrainDuration 	= 5 * time.Second
	defaultDiskThreshold	= 70.0 // represent % of the disk
	defaultCheckDiskSpace	= true

	httpError				= -1
)

var newLine = byte(10)

// LogzioSender instance of the
type LogzioSender struct {
	queue         	*goque.Queue
	drainDuration 	time.Duration
	buf           	*bytes.Buffer
	draining      	atomic.Bool
	mux           	sync.Mutex
	token         	string
	url           	string
	debug         	io.Writer
	diskThreshold 	float32
	checkDiskSpace 	bool
	dir           	string
}

// Sender Alias to LogzioSender
type Sender LogzioSender

// SenderOptionFunc options for logz
type SenderOptionFunc func(*LogzioSender) error

// New creates a new Logzio sender with a token and options
func New(token string, options ...SenderOptionFunc) (*LogzioSender, error) {
	l := &LogzioSender{
		buf:           	bytes.NewBuffer(make([]byte, maxSize)),
		drainDuration: 	defaultDrainDuration,
		url:           	fmt.Sprintf("%s/?token=%s", defaultHost, token),
		token:         	token,
		dir:           	fmt.Sprintf("%s%s%s%s%d", os.TempDir(), string(os.PathSeparator), "logzio-buffer", string(os.PathSeparator), time.Now().UnixNano()),
		diskThreshold: 	defaultDiskThreshold,
		checkDiskSpace: defaultCheckDiskSpace,
	}

	for _, option := range options {
		if err := option(l); err != nil {
			return nil, err
		}
	}
	q, err := goque.OpenQueue(l.dir)
	if err != nil {
		return nil, err
	}
	l.queue = q
	go l.start()
	return l, nil
}

// SetTempDirectory Use this temporary dir
func SetTempDirectory(dir string) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.dir = dir
		return nil
	}
}

// SetUrl set the url which maybe different from the defaultUrl
func SetUrl(url string) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.url = fmt.Sprintf("%s/?token=%s", url, l.token)
		l.debugLog("logziosender.go: Setting url to %s\n", l.url)
		return nil
	}
}

// SetDebug mode and send logs to this writer
func SetDebug(debug io.Writer) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.debug = debug
		return nil
	}
}

// SetDrainDuration to change the interval between drains
func SetDrainDuration(duration time.Duration) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.drainDuration = duration
		return nil
	}
}

// SetDrainDiskThreshold to change the maximum used disk space
func SetCheckDiskSpace(check bool) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.checkDiskSpace = check
		return nil
	}
}

// SetDrainDiskThreshold to change the maximum used disk space
func SetDrainDiskThreshold(th int) SenderOptionFunc {
	return func(l *LogzioSender) error {
		l.diskThreshold = float32(th)
		return nil
	}
}

func (l *LogzioSender) isEnoughDiskSpace() bool {
	if l.checkDiskSpace {
		usage := du.NewDiskUsage(l.dir)
		if usage.Usage()*100 > l.diskThreshold {
			l.debugLog("Logz.io: Dropping logs, as FS used space on %s is %g percent," +
				" and the drop threshold is %g percent",
				l.dir, usage.Usage()*100, l.diskThreshold)
			return false
		}
	}
	return true
}

// Send the payload to logz.io
func (l *LogzioSender) Send(payload []byte) error {
	if l.isEnoughDiskSpace() {
		_, err := l.queue.Enqueue(payload)
		return err
	}
	return nil
}

func (l *LogzioSender) start() {
	l.drainTimer()
}

// Stop will close the LevelDB queue and do a final drain
func (l *LogzioSender) Stop() {
	defer l.queue.Close()
	l.Drain()
}

func (l *LogzioSender) tryToSendLogs() int{
	// in case server side is sleeping - wait 10s instead of waiting for him to wake up
	client := &http.Client{
		Timeout: time.Second * 10,
	}
	resp, err := client.Post(l.url, "text/plain", l.buf)
	if err != nil {
		l.debugLog("logziosender.go: Error sending logs to %s %s\n", l.url, err)
		return httpError
	}
	//defer resp.Body.Close()
	statusCode := resp.StatusCode
	_, err = io.Copy(ioutil.Discard, io.LimitReader(resp.Body, respReadLimit))
	if err != nil {
		l.debugLog("Error reading response body: %v", err)
	}
	return statusCode
}

func (l *LogzioSender) drainTimer() {
	for {
		time.Sleep(l.drainDuration)
		l.Drain()
	}
}

func (l *LogzioSender) shouldRetry(attempt int, statusCode int) bool{
	retry := true
	switch statusCode {
	case http.StatusBadRequest:
		retry = false
	case http.StatusUnauthorized:
		retry = false
	case http.StatusOK:
		retry = false
	}

	if !retry && statusCode != http.StatusOK{
		l.requeue()
	}else if retry && attempt == (sendRetries - 1) {
		l.requeue()
	}
	return retry
}

// Drain - Send remaining logs
func (l *LogzioSender) Drain() {
	if l.draining.Load() {
		l.debugLog("logziosender.go: Already draining\n")
		return
	}
	l.mux.Lock()
	l.debugLog("logziosender.go: draining queue\n")
	defer l.mux.Unlock()
	l.draining.Toggle()
	defer l.draining.Toggle()
	var (
		err     error
		bufSize int
	)

	l.buf.Reset()
	bufSize = dequeueUpToMaxBatchSize(bufSize, err, l)
	if bufSize > 0 {
		backOff := sendSleepingBackoff
		toBackOff := false
		for attempt := 0; attempt < sendRetries; attempt++ {
			if toBackOff {
				time.Sleep(backOff)
				backOff *= 2
			}
			statusCode := l.tryToSendLogs()
			if l.shouldRetry(attempt, statusCode) {
				toBackOff = true
			} else {
				break
			}
		}
	}
}
func dequeueUpToMaxBatchSize(bufSize int, err error, l *LogzioSender) int {
	for bufSize < maxSize && err == nil {
		item, err := l.queue.Dequeue()
		if err != nil {
			l.debugLog("queue state: %s\n", err)
		}
		if item != nil {
			// NewLine is appended tp item.Value
			if len(item.Value)+bufSize+1 > maxSize {
				break
			}
			bufSize += len(item.Value)
			l.debugLog("logziosender.go: Adding item %d with size %d (total buffSize: %d)\n",
				item.ID, len(item.Value), bufSize)
			_, err := l.buf.Write(append(item.Value, newLine))
			if err != nil {
				l.errorLog("error writing to buffer %s", err)
			}
		} else {
			break
		}
	}
	return bufSize
}

// Sync drains the queue
func (l *LogzioSender) Sync() error {
	l.Drain()
	return nil
}

func (l *LogzioSender) requeue() {
	l.debugLog("logziosender.go: Requeue %s", l.buf.String())
	err := l.Send(l.buf.Bytes())
	if err != nil {
		l.errorLog("could not requeue logs %s", err)
	}
}

func (l *LogzioSender) debugLog(format string, a ...interface{}) {
	if l.debug != nil {
		fmt.Fprintf(l.debug, format, a...)
	}
}

func (l *LogzioSender) errorLog(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format, a...)
}

func (l *LogzioSender) Write(p []byte) (n int, err error) {
	return len(p), l.Send(p)
}
