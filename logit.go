package logit

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type loggerT struct {
	mu         sync.Mutex
	freeList   *buffer
	freeListMu sync.Mutex
	out        *bufio.Writer
	file       *os.File
	stderr     bool
	flag       uint32
}

const bufferSize = 256 * 1024

var l *loggerT

type Logger struct {
	level       Level
	crit        verbose
	alert       verbose
	err         verbose
	warning     verbose
	notice      verbose
	info        verbose
	debug       verbose
	infoPrefix  string
	debugPrefix string
}

var (
	pid      = os.Getpid()
	program  = filepath.Base(os.Args[0])
	host     = "unknownhost"
	userName = "unknownuser"
)

type Level uint32

const (
	Lcrit Level = iota
	Lalert
	Lfatal
	Lerr
	Lwarning
	Lnotice
	Linfo
	Ldebug
)

func (lev Level) String() string {
	switch lev {
	case Lcrit:
		return "CRIT"
	case Lalert:
		return "ALERT"
	case Lfatal:
		return "FATAL"
	case Lerr:
		return "ERROR"
	case Lwarning:
		return "WARNING"
	case Lnotice:
		return "NOTICE"
	case Linfo:
		return "INFO"
	case Ldebug:
		return "DEBUG"
	default:
		return ""
	}
}

type journald int

const (
	jcrit    journald = 0
	jalert            = 1
	jfatal            = 0
	jerr              = 3
	jwarning          = 4
	jnotice           = 5
	jinfo             = 6
	jdebug            = 6
)

const severityChar = "CAFEWNID"

var journalNum = map[Level]journald{
	Lcrit:    jcrit,
	Lfatal:   jfatal,
	Lalert:   jalert,
	Lerr:     jerr,
	Lwarning: jwarning,
	Lnotice:  jnotice,
	Linfo:    jinfo,
	Ldebug:   jdebug,
}

func (l *Level) set(level Level) {
	atomic.StoreUint32((*uint32)(l), uint32(level))
}

func (l *Level) get() Level {
	return Level(atomic.LoadUint32((*uint32)(l)))
}

type verbose bool

// v returns true if the Logger is configured at or above the given Level.
func (lg *Logger) v(level Level) verbose {
	if level <= lg.level {
		return true
	}

	return false
}

func (lg *Logger) SetVerbosity(level Level) {
	lg.level.set(level)
}

func (lg *Logger) Verbosity() Level {
	return lg.level.get()
}

const (
	Lstderr   uint32 = 1 << iota // Sets output to stderr.
	Lfile                        // Sets output to file.
	Ljournald                    // Sets output to have JournalD identifiers.
)

// flushDaemon periodically flushes the log file buffers at the given interval.
func (lt *loggerT) flushDaemon(flushInterval time.Duration) {
	for range time.NewTicker(flushInterval).C {
		lt.timeoutFlush(10 * time.Second)
	}
}

// New creates and returns a new Logger. Depending on the bitstring flag that
// is set, the logger will output log messages that are at or below the
// specified Level to the given file location and/or to stderr. The logger may
// be configured to output JournalD prefixes for color-coding within the
// `journald` facility. All options can be used together.
func New(file string, flushInterval time.Duration, level Level, flag uint32) (*Logger, error) {
	li := Logger{}
	li.level = level
	if l == nil {
		l = &loggerT{
			flag: flag,
		}

		if flag&Lfile == Lfile {
			f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0666)
			if err != nil {
				return nil, err
			}
			l.out = bufio.NewWriterSize(f, bufferSize)
			l.file = f
			go l.flushDaemon(flushInterval)
		}

		if flag&Lstderr == Lstderr {
			l.stderr = true
		}
	}

	return &li, nil
}

// Close does a delayed buffer flush and closes the log file.
func (lg *Logger) Close() {
	if l.out != nil {
		l.timeoutFlush(time.Second * 3)
		l.file.Close()
	}
}

// buffer holds a byte Buffer for reuse. The zero value is ready for use.
type buffer struct {
	bytes.Buffer
	tmp  [64]byte // temporary byte array for creating headers.
	next *buffer
}

// getBuffer returns a new, ready-to-use buffer.
func (lt *loggerT) getBuffer() *buffer {
	lt.freeListMu.Lock()
	b := lt.freeList
	if b != nil {
		lt.freeList = b.next
	}
	lt.freeListMu.Unlock()
	if b == nil {
		b = new(buffer)
	} else {
		b.next = nil
		b.Reset()
	}
	return b
}

// putBuffer returns a buffer to the free list.
func (lt *loggerT) putBuffer(b *buffer) {
	if b.Len() >= 256 {
		// Let big buffers die a natural death.
		return
	}
	lt.freeListMu.Lock()
	b.next = lt.freeList
	lt.freeList = b
	lt.freeListMu.Unlock()
}

// stacks is a wrapper for runtime.Stack that attempts to recover the data for all goroutines.
func stacks(all bool) []byte {
	// We don't know how big the traces are, so grow a few times if they don't fit. Start large, though.
	n := 10000
	if all {
		n = 100000
	}
	var trace []byte
	for i := 0; i < 5; i++ {
		trace = make([]byte, n)
		nbytes := runtime.Stack(trace, all)
		if nbytes < len(trace) {
			return trace[:nbytes]
		}
		n *= 2
	}
	return trace
}

// timeoutFlush calls Flush and returns when it completes or after timeout
// elapses, whichever happens first.  This is needed because the hooks invoked
// by Flush may deadlock when glog.Fatal is called from a hook that holds
// a lock.
func (lt *loggerT) timeoutFlush(timeout time.Duration) {
	done := make(chan bool, 1)
	go func() {
		lt.mu.Lock()
		lt.out.Flush()
		lt.mu.Unlock()
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Fprintln(os.Stderr, "logit: Flush took longer than", timeout)
	}
}

func (lt *loggerT) output(lev Level, buf *buffer, file string, line int) {
	lt.mu.Lock()
	data := buf.Bytes()
	if lt.file != nil {
		lt.out.Write(data)
	}
	if lt.stderr {
		os.Stderr.Write(data)
	}
	if lev == Lfatal {
		// Dump all goroutine stacks before exiting.
		// First, make sure we see the trace for the current goroutine on standard error.
		// If -logtostderr has been specified, the loop below will do that anyway
		// as the first stack in the full dump.
		if !lt.stderr {
			os.Stderr.Write(stacks(false))
		}
		// Write the stack trace for all goroutines to the files.
		trace := stacks(true)
		if lt.file != nil {
			lt.out.Write(trace)
		}
		if lt.stderr {
			os.Stderr.Write(trace)
		}
		lt.mu.Unlock()
		if lt.file != nil {
			lt.timeoutFlush(10 * time.Second)
			_ = lt.file.Close()
		}
		os.Exit(255)
	}
	lt.putBuffer(buf)
	lt.mu.Unlock()
}

func (lt *loggerT) print(lev Level, args ...interface{}) {
	lt.printDepth(lev, 0, args...)
}

func (lt *loggerT) printfDepth(lev Level, depth int, format string, args ...interface{}) {
	buf, file, line := lt.header(lev, depth)
	fmt.Fprintf(buf, format, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	lt.output(lev, buf, file, line)
}

func (lt *loggerT) printDepth(lev Level, depth int, args ...interface{}) {
	buf, file, line := lt.header(lev, depth)
	fmt.Fprint(buf, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	lt.output(lev, buf, file, line)
}

func (lt *loggerT) println(lev Level, args ...interface{}) {
	buf, file, line := lt.header(lev, 1)
	fmt.Fprintln(buf, args...)
	lt.output(lev, buf, file, line)
}

func (lt *loggerT) printf(lev Level, format string, args ...interface{}) {
	buf, file, line := lt.header(lev, 1)
	fmt.Fprintf(buf, format, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	lt.output(lev, buf, file, line)
}

// Some custom tiny helper functions to print the log header efficiently.
const digits = "0123456789"

// twoDigits formats a zero-prefixed two-digit integer at buf.tmp[i].
func (buf *buffer) twoDigits(i, d int) {
	buf.tmp[i+1] = digits[d%10]
	d /= 10
	buf.tmp[i] = digits[d%10]
}

// nDigits formats an n-digit integer at buf.tmp[i],
// padding with pad on the left.
// It assumes d >= 0.
func (buf *buffer) nDigits(n, i, d int, pad byte) {
	j := n - 1
	for ; j >= 0 && d > 0; j-- {
		buf.tmp[i+j] = digits[d%10]
		d /= 10
	}
	for ; j >= 0; j-- {
		buf.tmp[i+j] = pad
	}
}

// someDigits formats a zero-prefixed variable-width integer at buf.tmp[i].
func (buf *buffer) someDigits(i, d int) int {
	// Print into the top, then copy down. We know there's space for at least
	// a 10-digit number.
	j := len(buf.tmp)
	for {
		j--
		buf.tmp[j] = digits[d%10]
		d /= 10
		if d == 0 {
			break
		}
	}
	return copy(buf.tmp[i:], buf.tmp[j:])
}

func (lt *loggerT) header(lev Level, depth int) (*buffer, string, int) {
	_, file, line, ok := runtime.Caller(3 + depth)
	if !ok {
		file = "???"
		line = 1
	} else {
		slash := strings.LastIndex(file, "/")
		if slash >= 0 {
			file = file[slash+1:]
		}
	}
	return lt.formatHeader(lev, file, line), file, line
}

// formatHeader formats a log header using the provided file name and line number.
func (lt *loggerT) formatHeader(lev Level, file string, line int) *buffer {
	now := time.Now()
	if line < 0 {
		line = 0 // not a real line number, but acceptable to someDigits
	}
	buf := lt.getBuffer()

	// Avoid Fprintf, for speed. The format is so simple that we can do it quickly by hand.
	// It's worth about 3X. Fprintf is hard.
	_, month, day := now.Date()
	hour, minute, second := now.Clock()
	// Lmmdd hh:mm:ss.uuuuuu threadid file:line]
	if lt.flag&Ljournald == Ljournald {
		buf.tmp[0] = '<'
		buf.someDigits(1, int(journalNum[lev]))
		buf.tmp[2] = '>'
		buf.tmp[3] = ' '
	} else {
		buf.tmp[0] = ' '
		buf.tmp[1] = ' '
		buf.tmp[2] = ' '
		buf.tmp[3] = ' '
	}
	buf.tmp[4] = severityChar[lev]
	buf.twoDigits(5, int(month))
	buf.twoDigits(7, day)
	buf.tmp[9] = ' '
	buf.twoDigits(10, hour)
	buf.tmp[12] = ':'
	buf.twoDigits(13, minute)
	buf.tmp[15] = ':'
	buf.twoDigits(16, second)
	buf.tmp[18] = '.'
	buf.nDigits(6, 19, now.Nanosecond()/1000, '0')
	buf.tmp[25] = ' '
	buf.nDigits(7, 26, pid, ' ') // TODO: should be TID
	buf.tmp[33] = ' '
	buf.Write(buf.tmp[:34])
	buf.WriteString(file)
	buf.tmp[0] = ':'
	n := buf.someDigits(1, line)
	buf.tmp[n+1] = ']'
	buf.tmp[n+2] = ' '
	buf.Write(buf.tmp[:n+3])
	return buf
}

// Crit logs a message if verbosity is set appropriately.
func (v verbose) Crit(msg string) {
	if v {
		l.println(Lcrit, msg)
	}
}

// Critf logs a formatted message if verbosity is set appropriately.
func (v verbose) Critf(fmt string, args ...interface{}) {
	if v {
		l.printf(Lcrit, fmt, args...)
	}
}

// Alert logs a message if verbosity is set appropriately.
func (v verbose) Alert(msg string) {
	if v {
		l.println(Lalert, msg)
	}
}

// Alertf logs a message if verbosity is set appropriately.
func (v verbose) Alertf(fmt string, args ...interface{}) {
	if v {
		l.printf(Lalert, fmt, args...)
	}
}

// Error logs a message if verbosity is set appropriately.
func (v verbose) Error(msg string) {
	if v {
		l.println(Lerr, msg)
	}
}

// Errorf logs a message if verbosity is set appropriately.
func (v verbose) Errorf(fmt string, args ...interface{}) {
	if v {
		l.printf(Lerr, fmt, args...)
	}
}

// Warn logs a message if verbosity is set appropriately.
func (v verbose) Warn(msg string) {
	if v {
		l.println(Lwarning, msg)
	}
}

// Warnf logs a message if verbosity is set appropriately.
func (v verbose) Warnf(fmt string, args ...interface{}) {
	if v {
		l.printf(Lwarning, fmt, args...)
	}
}

// Notice logs a message if verbosity is set appropriately.
func (v verbose) Notice(msg string) {
	if v {
		l.println(Lnotice, msg)
	}
}

// Noticef logs a message if verbosity is set appropriately.
func (v verbose) Noticef(fmt string, args ...interface{}) {
	if v {
		l.printf(Lnotice, fmt, args...)
	}
}

// Info logs a message if verbosity is set appropriately.
func (v verbose) Info(msg string) {
	if v {
		l.println(Linfo, msg)
	}
}

// Infof logs a message if verbosity is set appropriately.
func (v verbose) Infof(fmt string, args ...interface{}) {
	if v {
		l.printf(Linfo, fmt, args...)
	}
}

// Debug logs a message if verbosity is set appropriately.
func (v verbose) Debug(msg string) {
	if v {
		l.println(Ldebug, msg)
	}
}

// Debugf logs a message if verbosity is set appropriately.
func (v verbose) Debugf(fmt string, args ...interface{}) {
	if v {
		l.printf(Ldebug, fmt, args...)
	}
}

// Crit logs a message if verbosity is set appropriately.
func (lg *Logger) Crit(msg string) {
	v := lg.v(Lcrit)
	v.Crit(msg)
}

// Critf logs a message if verbosity is set appropriately.
func (lg *Logger) Critf(fmt string, args ...interface{}) {
	v := lg.v(Lcrit)
	v.Critf(fmt, args...)
}

// Alert logs a message if verbosity is set appropriately.
func (lg *Logger) Alert(msg string) {
	v := lg.v(Lalert)
	v.Alert(msg)
}

// Alertf logs a message if verbosity is set appropriately.
func (lg *Logger) Alertf(fmt string, args ...interface{}) {
	v := lg.v(Lalert)
	v.Alertf(fmt, args...)
}

// Fatal logs a message and terminates the application.
func (lg *Logger) Fatal(msg string) {
	l.printDepth(Lfatal, 0, msg)
}

// Fatalf logs a formatted message and terminates the application.
func (lg *Logger) Fatalf(fmt string, args ...interface{}) {
	l.printfDepth(Lfatal, 0, fmt, args...)
}

// Error logs a message if verbosity is set appropriately.
func (lg *Logger) Error(msg string) {
	v := lg.v(Lerr)
	v.Error(msg)
}

// Errorf logs a message if verbosity is set appropriately.
func (lg *Logger) Errorf(fmt string, args ...interface{}) {
	v := lg.v(Lerr)
	v.Errorf(fmt, args...)
}

// Warn logs a message if verbosity is set appropriately.
func (lg *Logger) Warn(msg string) {
	v := lg.v(Lwarning)
	v.Warn(msg)
}

// Warnf logs a message if verbosity is set appropriately.
func (lg *Logger) Warnf(fmt string, args ...interface{}) {
	v := lg.v(Lwarning)
	v.Warnf(fmt, args...)
}

// Notice logs a message if verbosity is set appropriately.
func (lg *Logger) Notice(msg string) {
	v := lg.v(Lnotice)
	v.Notice(msg)
}

// Noticef logs a message if verbosity is set appropriately.
func (lg *Logger) Noticef(fmt string, args ...interface{}) {
	v := lg.v(Lnotice)
	v.Noticef(fmt, args...)
}

// Info logs a message if verbosity is set appropriately.
func (lg *Logger) Info(msg string) {
	v := lg.v(Linfo)
	v.Info(msg)
}

// Infof logs a message if verbosity is set appropriately.
func (lg *Logger) Infof(fmt string, args ...interface{}) {
	v := lg.v(Linfo)
	v.Infof(fmt, args...)
}

// Debug logs a message if verbosity is set appropriately.
func (lg *Logger) Debug(msg string) {
	v := lg.v(Ldebug)
	v.Debug(msg)
}

// Debugf logs a message if verbosity is set appropriately.
func (lg *Logger) Debugf(fmt string, args ...interface{}) {
	v := lg.v(Ldebug)
	v.Debugf(fmt, args...)
}
