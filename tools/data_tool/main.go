package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/absolute8511/glog"
	"github.com/absolute8511/redigo/redis"
	"github.com/youzan/ZanRedisDB/common"
	"github.com/youzan/ZanRedisDB/rockredis"
)

var ipProxy = flag.String("ip_proxy", "127.0.0.1", "server ip")
var portProxy = flag.Int("port_proxy", 3803, "server port")
var toolType = flag.String("type", "", "data tool type: gen_delrange/compactdb/scan_localttl")
var ns = flag.String("ns", "", "namespace for data")
var table = flag.String("table", "", "table for data")
var key = flag.String("key", "", "key for action")
var cleanMatched = flag.Bool("clean_matched", false, "do del to proxy if matched")
var maxScan = flag.Int("max_scan", 10000, "max scan db keys")
var sleepBetween = flag.Duration("sleep_between", time.Millisecond, "")
var startFrom = flag.String("start_from", "", "key range for data")
var endTo = flag.String("end_to", "", "key range for data")
var dbFile = flag.String("dbfile", "", "file path for rocksdb parent, the final will be dbFile/rocksdb")

type DeleteTableRange struct {
	Table     string `json:"table,omitempty"`
	StartFrom []byte `json:"start_from,omitempty"`
	EndTo     []byte `json:"end_to,omitempty"`
	// to avoid drop all table data, this is needed to delete all data in table
	DeleteAll bool `json:"delete_all,omitempty"`
	Dryrun    bool `json:"dryrun,omitempty"`
}

func getRangeStr(dr DeleteTableRange) string {
	b, _ := json.Marshal(dr)
	return string(b)
}

func main() {
	flag.Parse()
	common.InitDefaultForGLogger("./")
	defer glog.Flush()

	switch *toolType {
	case "gen_delrange":
		log.Printf("test gen del range")
		dr := DeleteTableRange{
			StartFrom: []byte(*startFrom),
			EndTo:     []byte(*endTo),
		}
		log.Printf("%s", getRangeStr(dr))
	case "compact_db":
		compactDB()
	case "scan_localttl":
		checkLocalTTL()
	default:
		log.Printf("unknown action: %v", *toolType)
	}
}

var (
	errExpTimeKey = errors.New("invalid expire time key")
)

const (
	logTimeFormatStr = "2006-01-02 15:04:05"
)

/*
the coded format of expire time key:
bytes:  -0-|-1-2-3-4-5-6-7-8-|----9---|-10-11--------x-|
data :  103|       when      |dataType|       key      |
*/
func expEncodeTimeKey(dataType byte, key []byte, when int64) []byte {
	buf := make([]byte, len(key)+1+8+1)

	pos := 0
	buf[pos] = rockredis.ExpTimeType
	pos++

	binary.BigEndian.PutUint64(buf[pos:], uint64(when))
	pos += 8

	buf[pos] = dataType
	pos++

	copy(buf[pos:], key)

	return buf
}

//decode the expire 'time key', the return values are: dataType, key, whenToExpire, error
func expDecodeTimeKey(tk []byte) (byte, []byte, int64, error) {
	pos := 0
	if pos+10 > len(tk) || tk[pos] != rockredis.ExpTimeType {
		return 0, nil, 0, errExpTimeKey
	}

	return tk[pos+9], tk[pos+10:], int64(binary.BigEndian.Uint64(tk[pos+1:])), nil
}

func checkLocalTTL() {
	log.Printf("begin check ttl")
	now := time.Now().Unix() - 3600

	cfg := rockredis.NewRockRedisDBConfig()
	cfg.DataDir = *dbFile
	cfg.ReadOnly = true
	cfg.DataTool = true
	db, err := rockredis.OpenRockDB(cfg)
	if err != nil {
		log.Printf("open db failed: %s", err.Error())
		return
	}
	defer db.Close()
	minKey := expEncodeTimeKey(rockredis.NoneType, nil, 0)
	maxKey := expEncodeTimeKey(100, nil, now)

	var eCount int64
	var scanned int64
	checkStart := time.Now()

	it, err := db.NewDBRangeLimitIterator(minKey, maxKey,
		common.RangeROpen, 0, -1, false)
	if err != nil {
		log.Printf("open db iterator failed: %s", err.Error())
		return
	}
	defer it.Close()

	clientConn, err := redis.Dial("tcp", net.JoinHostPort(*ipProxy, strconv.Itoa(*portProxy)), redis.DialConnectTimeout(time.Second))
	if *cleanMatched {
		if err != nil {
			log.Printf("connect redis failed: %s", err.Error())
			return
		}
	}

	missed := 0
	cleaned := 0
	for ; it.Valid(); it.Next() {
		if scanned > int64(*maxScan) {
			break
		}
		tk := it.Key()
		if tk == nil {
			continue
		}

		dt, k, nt, dErr := expDecodeTimeKey(tk)
		if dErr != nil {
			continue
		}

		scanned += 1
		if nt > now {
			//the next ttl check time is nt!
			log.Printf("ttl check end at key:[%s] of type:%s whose expire time is: %s", string(k),
				rockredis.TypeName[dt], time.Unix(nt, 0).Format(logTimeFormatStr))
			break
		}

		eCount += 1

		if *key != "" {
			if strings.HasPrefix(string(k), *key) {
				log.Printf("found key %s(type: %v), expire time: %s\n", string(k), rockredis.TypeName[dt],
					time.Unix(nt, 0).Format(logTimeFormatStr))
				if *cleanMatched && dt == rockredis.KVType {
					delKey := fmt.Sprintf("%s:%s", *ns, string(k))
					n, err := redis.Int(clientConn.Do("exists", delKey))
					if err != nil {
						log.Printf("del %s failed: %s", delKey, err.Error())
					} else if n == 1 {
						n, err := redis.Int(clientConn.Do("del", delKey))
						if err != nil {
							log.Printf("del %s failed: %s", delKey, err.Error())
						} else if n == 0 {
							log.Printf("del %s not exist: %v", delKey, n)
							missed++
						} else if n == 1 {
							cleaned++
						}
						if *sleepBetween > 0 {
							time.Sleep(*sleepBetween)
						}
					} else if n == 0 {
						missed++
					}
				}
			}
			continue
		}
		log.Printf("scanned ttl: key %s(type: %v), expire time: %s\n", string(k), rockredis.TypeName[dt],
			time.Unix(nt, 0).Format(logTimeFormatStr))
	}

	checkCost := time.Since(checkStart)
	log.Printf("[%d/%d] keys have expired during ttl checking, cost:%s, clean %v, %v", eCount, scanned, checkCost, cleaned, missed)
}

func compactDB() {
	cfg := rockredis.NewRockRedisDBConfig()
	cfg.DataDir = *dbFile
	db, err := rockredis.OpenRockDB(cfg)
	if err != nil {
		log.Printf("open db failed: %s", err.Error())
		return
	}
	defer db.Close()
	db.CompactAllRange()
}
