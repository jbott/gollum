package producer

import (
	"bytes"
	elastigo "github.com/mattbaird/elastigo/lib"
	"github.com/trivago/gollum/log"
	"github.com/trivago/gollum/shared"
	"strconv"
	"sync"
	"time"
)

// ElasticSearch producer plugin
// Configuration example
//
//   - "producer.ElasticSearch":
//     Enable: true
//     Connections: 10
//     Port: 9200
//     User: "root"
//     Password: "root"
//     BatchSizeByte: 65535
//     BatchMaxCount: 512
//     BatchTimeoutSec: 5
//     RetrySec: 5
//     TTL: "1d"
//     Stream:
//       - "console"
//       - "_GOLLUM_"
//     Servers:
//       - "localhost"
//     Index:
//       "console" : "default"
//       "_GOLLUM_"  : "default"
//     Type:
//       "console" : "log"
//       "_GOLLUM_"  : "gollum"
//
// Servers defines a list of servers to connect to. The first server in the list
// is used as the server passed to the "Domain" setting. The Domain setting can
// be overwritten, too.
//
// Port defines the elasticsearch port, wich has to be the same for all servers.
// By default this is set to 9200.
//
// User and Password can be used to pass credentials to the elasticsearch server.
// By default both settings are empty.
//
// Index maps a stream to a specific index. You can define the
// wildcard stream (*) here, too. All streams that do not have a specific
// mapping will go to this stream (including _GOLLUM_).
// If no category mappings are set all messages will be send to "default".
//
// Type maps a stream to a specific type. This behaves like the index map and
// is used to assign a _type to an elasticsearch message. By default the topic
// "log" is used.
//
// BatchSizeByte defines the size in bytes required to trigger a flush.
// By default this is set to 32768 (32KB).
//
// BatchMaxCount defines the number of documents required to trigger a flush.
// By default this is set to 256.
//
// BatchTimeoutSec defines the time in seconds after which a flush will be
// triggered. By default this is set to 5.
//
// RetrySec denotes the time in seconds after which a failed dataset will be
// transmitted again. By default this is set to 5.
//
// Connections defines the number of simultaneous connections allowed to a
// elasticsearch server. This is set to 6 by default.
//
// TTL defines the TTL set in elasticsearch messages. By default this is set to
// "" which means no TTL.
type ElasticSearch struct {
	shared.ProducerBase
	conn          *elastigo.Conn
	indexer       *elastigo.BulkIndexer
	index         map[shared.MessageStreamID]string
	msgType       map[shared.MessageStreamID]string
	msgTTL        string
	dayBasedIndex bool
}

func init() {
	shared.RuntimeType.Register(ElasticSearch{})
}

// Configure initializes this producer with values from a plugin config.
func (prod *ElasticSearch) Configure(conf shared.PluginConfig) error {
	// If not defined, delimiter is not used (override default value)
	if !conf.HasValue("Delimiter") {
		conf.Override("Delimiter", "")
	}

	err := prod.ProducerBase.Configure(conf)
	if err != nil {
		return err
	}

	defaultServer := []string{"localhost"}
	numConnections := conf.GetInt("Connections", 6)
	retrySec := conf.GetInt("RetrySec", 5)

	prod.conn = elastigo.NewConn()
	prod.conn.Hosts = conf.GetStringArray("Servers", defaultServer)
	prod.conn.Domain = conf.GetString("Domain", prod.conn.Hosts[0])
	prod.conn.ClusterDomains = prod.conn.Hosts
	prod.conn.Port = strconv.Itoa(conf.GetInt("Port", 9200))
	prod.conn.Username = conf.GetString("User", "")
	prod.conn.Password = conf.GetString("Password", "")

	prod.indexer = prod.conn.NewBulkIndexerErrors(numConnections, retrySec)
	prod.indexer.BufferDelayMax = time.Duration(conf.GetInt("BatchTimeoutSec", 5)) * time.Second
	prod.indexer.BulkMaxBuffer = conf.GetInt("BatchSizeByte", 32768)
	prod.indexer.BulkMaxDocs = conf.GetInt("BatchMaxCount", 128)

	prod.indexer.Sender = func(buf *bytes.Buffer) error {
		_, err := prod.conn.DoCommand("POST", "/_bulk", nil, buf)
		if err != nil {
			Log.Error.Print("ElasticSearch response error - ", err)
		}
		return err
	}

	prod.index = conf.GetStreamMap("Index", "default")
	prod.msgType = conf.GetStreamMap("Type", "log")
	prod.msgTTL = conf.GetString("TTL", "")
	prod.dayBasedIndex = conf.GetBool("DayBasedIndex", false)

	return nil
}

func (prod *ElasticSearch) sendMessage(msg shared.Message) {
	index, indexMapped := prod.index[msg.CurrentStream]
	if !indexMapped {
		index = prod.index[shared.WildcardStreamID]
	}

	if prod.dayBasedIndex {
		index = index + "_" + msg.Timestamp.Format("2006-01-02")
	}

	msgType, typeMapped := prod.msgType[msg.CurrentStream]
	if !typeMapped {
		msgType = prod.msgType[shared.WildcardStreamID]
	}

	prod.Formatter().PrepareMessage(msg)
	err := prod.indexer.Index(index, msgType, "", prod.msgTTL, &msg.Timestamp, prod.Formatter().String(), true)
	if err != nil {
		Log.Error.Print("ElasticSearch index error - ", err)
	}
}

// Produce starts a bluk indexer
func (prod ElasticSearch) Produce(threads *sync.WaitGroup) {
	prod.indexer.Start()
	defer func() {
		prod.indexer.Flush()
		prod.indexer.Stop()
		prod.MarkAsDone()
	}()

	prod.DefaultControlLoop(threads, prod.sendMessage, nil)
}
