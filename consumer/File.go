package consumer

import (
	"github.com/trivago/gollum/log"
	"github.com/trivago/gollum/shared"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fileBufferGrowSize = 1024
	fileOffsetStart    = "Start"
	fileOffsetEnd      = "End"
	fileOffsetContinue = "Current"
)

type fileState int32

const (
	fileStateOpen = fileState(iota)
	fileStateRead = fileState(iota)
	fileStateDone = fileState(iota)
)

// File consumer plugin
// Configuration example
//
//   - "consumer.File":
//     Enable: true
//     File: "test.txt"
//     Offset: "Current"
//     Delimiter: "\n"
//
// File is a mandatory setting and contains the file to read. The file will be
// read from beginning to end and the reader will stay attached until the
// consumer is stopped. This means appends to the file will be recognized by
// gollum. Symlinks are always resolved, i.e. changing the symlink target will
// be ignored unless gollum is restarted.
//
// Offset defines where to start reading the file. Valid values (case sensitive)
// are "Start", "End", "Current". By default this is set to "End". If "Current"
// is used a filed in /tmp will be created that contains the last position that
// has been read.
//
// Delimiter defines the end of a message inside the file. By default this is
// set to "\n".
type File struct {
	shared.ConsumerBase
	file             *os.File
	fileName         string
	continueFileName string
	delimiter        string
	seek             int
	seekOffset       int64
	persistSeek      bool
	state            fileState
}

func init() {
	shared.RuntimeType.Register(File{})
}

// Configure initializes this consumer with values from a plugin config.
func (cons *File) Configure(conf shared.PluginConfig) error {
	err := cons.ConsumerBase.Configure(conf)
	if err != nil {
		return err
	}

	if !conf.HasValue("File") {
		return shared.NewConsumerError("No file configured for consumer.File")
	}

	escapeChars := strings.NewReplacer("\\n", "\n", "\\r", "\r", "\\t", "\t")

	cons.file = nil
	cons.fileName = conf.GetString("File", "")
	cons.delimiter = escapeChars.Replace(conf.GetString("Delimiter", "\n"))
	cons.persistSeek = false

	switch conf.GetString("Offset", fileOffsetEnd) {
	default:
		fallthrough
	case fileOffsetEnd:
		cons.seek = 2
		cons.seekOffset = 0

	case fileOffsetStart:
		cons.seek = 1
		cons.seekOffset = 0

	case fileOffsetContinue:
		cons.seek = 1
		cons.seekOffset = 0
		cons.persistSeek = true
	}

	return nil
}

func (cons *File) postAndPersist(data []byte, sequence uint64) {
	cons.seekOffset, _ = cons.file.Seek(0, 1)
	cons.PostMessageFromSlice(data, sequence)
	ioutil.WriteFile(cons.continueFileName, []byte(strconv.FormatInt(cons.seekOffset, 10)), 0644)
}

func (cons *File) realFileName() string {
	baseFileName, err := filepath.EvalSymlinks(cons.fileName)
	if err != nil {
		baseFileName = cons.fileName
	}

	baseFileName, err = filepath.Abs(baseFileName)
	if err != nil {
		baseFileName = cons.fileName
	}

	return baseFileName
}

func (cons *File) setState(state fileState) {
	cons.state = state
}

func (cons *File) initFile() {
	defer cons.setState(fileStateRead)

	if cons.file != nil {
		cons.file.Close()
		cons.file = nil
	}

	if cons.persistSeek {
		baseFileName := cons.realFileName()
		pathDelimiter := strings.NewReplacer("/", "_", ".", "_")
		cons.continueFileName = "/tmp/gollum" + pathDelimiter.Replace(baseFileName) + ".idx"
		cons.seekOffset = 0

		fileContents, err := ioutil.ReadFile(cons.continueFileName)
		if err == nil {
			cons.seekOffset, err = strconv.ParseInt(string(fileContents), 10, 64)
		}
	}
}

func (cons *File) read() {
	defer func() {
		if cons.file != nil {
			cons.file.Close()
		}
		cons.MarkAsDone()
	}()

	var buffer shared.BufferedReader
	if cons.persistSeek {
		buffer = shared.NewBufferedReader(fileBufferGrowSize, 0, cons.delimiter, cons.postAndPersist)
	} else {
		buffer = shared.NewBufferedReader(fileBufferGrowSize, 0, cons.delimiter, cons.PostMessageFromSlice)
	}

	printFileOpenError := true
	for cons.state != fileStateDone {
		// Initialize the seek state if requested
		// Try to read the remains of the file first
		if cons.state == fileStateOpen {
			if cons.file != nil {
				buffer.Read(cons.file)
			}
			cons.initFile()
			buffer.Reset(uint64(cons.seekOffset))
		}

		// Try to open the file to read from
		if cons.state == fileStateRead && cons.file == nil {
			file, err := os.OpenFile(cons.realFileName(), os.O_RDONLY, 0666)

			switch {
			case err != nil:
				if printFileOpenError {
					Log.Error.Print("File open error - ", err)
					printFileOpenError = false
				}
				time.Sleep(3 * time.Second)
				continue
			default:
				cons.file = file
				cons.seekOffset, _ = cons.file.Seek(cons.seekOffset, cons.seek)
				printFileOpenError = true
			}
		}

		// Try to read from the file
		if cons.state == fileStateRead && cons.file != nil {
			err := buffer.Read(cons.file)

			switch {
			case err == nil: // ok
			case err == io.EOF:
				runtime.Gosched()
			case cons.state == fileStateRead:
				Log.Error.Print("Error reading file - ", err)
				cons.file.Close()
				cons.file = nil
			}
		}
	}
}

// Consume listens to stdin.
func (cons File) Consume(threads *sync.WaitGroup) {
	cons.setState(fileStateOpen)
	defer cons.setState(fileStateDone)

	go cons.read()
	cons.DefaultControlLoop(threads, func() { cons.setState(fileStateOpen) })
}
