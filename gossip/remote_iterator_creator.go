package gossip

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/influxdata/influxdb/influxql"
	"github.com/spf13/viper"
)

// RemoteIteratorCreator implements influxql.IteratorCreator
type RemoteIteratorCreator struct {
	Store   *TSDBStore
	ShardID uint64
	NodeID  uint64
	// ShardIDs []uint64
}

// NodesList is used to return list of nodes
type NodesList struct {
	ID          uint64 `json:"id"`
	IP          string `json:"ip"`
	Hostname    string `json:"hostname"`
	BindAddress string `json:"bindAddress"`
	Alive       bool   `json:"alive"`
}

// CreateIterator Creates a simple iterator for use in an InfluxQL query for the remote node
func (ric *RemoteIteratorCreator) CreateIterator(opt influxql.IteratorOptions) (influxql.Iterator, error) {
	aliveNodes, err := AliveNodesMap()
	log.Printf("************* NodeID = %d, aliveNodes = %+v", ric.NodeID, aliveNodes[ric.NodeID])

	optBinary, err := opt.MarshalBinary()
	if err != nil {
		log.Printf("Error while marshaling IteratorOptions: %s", err.Error())
		return nil, err
	}

	cmd := &ReadShardCommand{
		ShardID:         ric.ShardID,
		IteratorOptions: optBinary,
	}

	data, err := proto.Marshal(cmd)
	if err != nil {
		log.Printf("Error while marshaling ReadShardCommand: %s", err.Error())
		return nil, err
	}

	f := func() (*http.Request, error) {
		url := "http://" + aliveNodes[ric.NodeID].BindAddress + "/read"
		return http.NewRequest("POST", url, bytes.NewBuffer(data))
	}

	resp, err := ExpBackoffRequest(f)
	if err != nil {
		log.Printf("Failed to read shards from remote node with ID: %d", ric.NodeID)
		return nil, err
	}

	respMessage := &ReadShardCommandResponse{}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed while reading received http body: %s", err.Error())
		return nil, err
	}
	err = proto.Unmarshal(respBody, respMessage)
	if err != nil {
		log.Printf("Error while unmarshaling response: %s", err.Error())
		return nil, err
	}

	dec := influxql.NewPointDecoder(resp.Body)
	var iter influxql.Iterator
	iterType := respMessage.Type
	closeBody := func() {
		resp.Body.Close()
	}
	switch iterType {
	case ReadShardCommandResponse_FLOAT:
		iter = &RemoteFloatIterator{PointDecoder: dec, Closed: false, CloseReader: closeBody}
	case ReadShardCommandResponse_INTEGER:
		iter = &RemoteIntegerIterator{PointDecoder: dec, Closed: false, CloseReader: closeBody}
	case ReadShardCommandResponse_STRING:
		iter = &RemoteStringIterator{PointDecoder: dec, Closed: false, CloseReader: closeBody}
	case ReadShardCommandResponse_BOOLEAN:
		iter = &RemoteBooleanIterator{PointDecoder: dec, Closed: false, CloseReader: closeBody}
	default:
		return nil, fmt.Errorf("Unsupported iterator type: %d", iterType)
	}

	return iter, nil
}

// FieldDimensions Returns the unique fields and dimensions across a list of sources from the remote node
func (ric *RemoteIteratorCreator) FieldDimensions(sources influxql.Sources) (fields map[string]influxql.DataType, dimensions map[string]struct{}, err error) {
	sourceBinary, err := sources.MarshalBinary()
	if err != nil {
		return nil, nil, err
	}
	fdc := &FieldDimensionsCommand{ShardID: ric.ShardID, Sources: sourceBinary}
	fdcBinary, err := proto.Marshal(fdc)
	if err != nil {
		log.Printf("Error while marshaling FieldDimensionsCommand: %s", err.Error())
		return nil, nil, err
	}

	aliveNodes, err := AliveNodesMap()
	if err != nil {
		return nil, nil, err
	}
	f := func() (*http.Request, error) {
		log.Printf("ric.NodeID=%d, aliveNodes[ric.NodeID]=%+v", ric.NodeID, aliveNodes[ric.NodeID])
		url := "http://" + aliveNodes[ric.NodeID].BindAddress + "/fielddimensions"
		return http.NewRequest("POST", url, bytes.NewBuffer(fdcBinary))
	}

	resp, err := ExpBackoffRequest(f)
	if err != nil {
		return nil, nil, err
	}

	respMessage := &FieldDimensionsCommandResponse{}
	respBody, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		log.Printf("Failed while reading received http body: %s", err.Error())
		return nil, nil, err
	}
	err = proto.Unmarshal(respBody, respMessage)
	if err != nil {
		log.Printf("Error while unmarshaling response: %s", err.Error())
		return nil, nil, err
	}

	if respMessage.Error != "" {
		return nil, nil, errors.New(respMessage.Error)
	}

	fields = make(map[string]influxql.DataType)
	for key, value := range respMessage.GetFields() {
		fields[key] = influxql.DataType(value)
	}

	dimensions = make(map[string]struct{})
	for _, dimension := range respMessage.Dimensions {
		dimensions[dimension] = struct{}{}
	}

	return fields, dimensions, nil
}

// ExpandSources Expands regex sources to all matching sources for the remote nodes
func (ric *RemoteIteratorCreator) ExpandSources(sources influxql.Sources) (influxql.Sources, error) {
	sourceBinary, err := sources.MarshalBinary()
	if err != nil {
		return nil, err
	}
	cmd := &ExpandSourcesCommand{ShardID: ric.ShardID, Sources: sourceBinary}
	cmdBinary, err := proto.Marshal(cmd)
	if err != nil {
		log.Printf("Error while marshaling ExpandSourcesCommand: %s", err.Error())
		return nil, err
	}

	aliveNodes, err := AliveNodesMap()
	if err != nil {
		return nil, err
	}

	f := func() (*http.Request, error) {
		url := "http://" + aliveNodes[ric.NodeID].BindAddress + "/expandsources"
		return http.NewRequest("POST", url, bytes.NewBuffer(cmdBinary))
	}
	resp, err := ExpBackoffRequest(f)
	if err != nil {
		return nil, err
	}

	respMessage := &ExpandSourcesCommandResponse{}
	respBody, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		log.Printf("Failed while reading received http body: %s", err.Error())
		return nil, err
	}
	err = proto.Unmarshal(respBody, respMessage)
	if err != nil {
		log.Printf("Error while unmarshaling response: %s", err.Error())
		return nil, err
	}
	if respMessage.Error != "" {
		return nil, errors.New(respMessage.Error)
	}
	var respSources influxql.Sources
	err = respSources.UnmarshalBinary(respMessage.Sources)
	if err != nil {
		return nil, err
	}

	return respSources, nil
}

// AliveNodesMap foo
func AliveNodesMap() (map[uint64]NodesList, error) {
	f := func() (*http.Request, error) {
		url := viper.GetString("CFLUX_ENDPOINT") + "/nodes/" + url.QueryEscape(viper.GetString("CLUSTER"))
		return http.NewRequest("GET", url, nil)
	}
	resp, err := ExpBackoffRequest(f)
	if err != nil {
		return nil, err
	}
	var nodeList []NodesList
	nodeMap := map[uint64]NodesList{}
	err = json.NewDecoder(resp.Body).Decode(&nodeList)
	if err != nil {
		return nil, err
	}
	for _, node := range nodeList {
		nodeMap[node.ID] = node
		log.Printf("***** assign alive to %d = %+v", node.ID, node)
	}
	return nodeMap, nil
}

// ExpBackoffRequest foo
func ExpBackoffRequest(f func() (*http.Request, error)) (*http.Response, error) {
	client := &http.Client{}
	var resp *http.Response
	var req *http.Request
	var err error

	for attempt := 1; attempt < 6; attempt++ {
		req, err = f()
		// log.Printf("req=%+v", req)
		if err != nil {
			return nil, err
		}
		resp, err = client.Do(req)
		if err == nil {
			return resp, err
		}
		backoff := (math.Pow(2, float64(attempt)) - 1) / 2
		time.Sleep(time.Duration(backoff) * time.Second)
	}
	log.Printf("Error while connecting to Clusterflux: %s", err.Error())
	return resp, err
}
