package pdnode_coord

import (
	"encoding/json"
	. "github.com/absolute8511/ZanRedisDB/cluster"
	"github.com/absolute8511/ZanRedisDB/common"
	"net"
	"time"
)

func getIndexSchemasFromDataNode(remoteNode string, ns string, table string) (map[string]*common.IndexSchema, error) {
	nip, _, _, httpPort := ExtractNodeInfoFromID(remoteNode)
	rsp := make(map[string]*common.IndexSchema)
	_, err := common.APIRequest("GET",
		"http://"+net.JoinHostPort(nip, httpPort)+common.APIGetIndexes+"/"+ns+"/"+table,
		nil, time.Second*3, &rsp)
	if err != nil {
		CoordLog().Infof("failed (%v) to get indexes for namespace %v table %v: %v",
			nip, ns, table, err)
		return nil, err
	}
	return rsp, nil
}

func isAllPartsIndexSchemaReady(allPartsSchema map[int]map[string]*common.IndexSchema, table string,
	name string, expectedState common.IndexState) bool {
	allDone := true
	for _, partSchema := range allPartsSchema {
		s, ok := partSchema[table]
		if !ok {
			allDone = false
			break
		}
		isSame := false
		for _, partIndex := range s.HsetIndexes {
			if partIndex.Name == name {
				if partIndex.State == expectedState {
					isSame = true
				}
				break
			}
		}
		if !isSame {
			allDone = false
			break
		}
	}
	return allDone
}

func (self *PDCoordinator) doSchemaCheck() {
	allNamespaces, _, err := self.register.GetAllNamespaces()
	if err != nil {
		return
	}
	for ns, parts := range allNamespaces {
		schemas, err := self.register.GetNamespaceSchemas(ns)
		if err != nil {
			if err != ErrKeyNotFound {
				CoordLog().Infof("get schema info failed: %v", err)
			}
			continue
		}
		if len(parts) == 0 || len(parts) != parts[0].PartitionNum {
			continue
		}
		allPartsSchema := make(map[int]map[string]*common.IndexSchema)
		isReady := true
		for pid, part := range parts {
			if len(part.RaftNodes) == 0 {
				isReady = false
				break
			}
			schemas, err := getIndexSchemasFromDataNode(part.RaftNodes[0], ns, "")
			if err != nil {
				isReady = false
				break
			}
			allPartsSchema[pid] = schemas
		}
		if !isReady || len(allPartsSchema) != parts[0].PartitionNum {
			continue
		}
		for table, schemaInfo := range schemas {
			var indexes common.IndexSchema
			err := json.Unmarshal(schemaInfo.Schema, &indexes)
			if err != nil {
				CoordLog().Infof("unmarshal schema data failed: %v", err)
				continue
			}
			schemaChanged := false
			newSchemaInfo := schemaInfo
			for _, hindex := range indexes.HsetIndexes {
				switch hindex.State {
				case common.InitIndex:
					// wait all partitions to begin init index, and then change the state to building
					if isAllPartsIndexSchemaReady(allPartsSchema, table, hindex.Name, common.InitIndex) {
						schemaChanged = true
						hindex.State = common.BuildingIndex
					}
				case common.BuildingIndex:
					// wait all partitions to finish building index, and then change the state to ready
					if isAllPartsIndexSchemaReady(allPartsSchema, table, hindex.Name, common.BuildDoneIndex) {
						schemaChanged = true
						hindex.State = common.ReadyIndex
					}
				default:
				}
			}
			for _, jsonIndex := range indexes.JsonIndexes {
				_ = jsonIndex
			}

			if schemaChanged {
				err := self.register.UpdateNamespaceSchema(ns, table, newSchemaInfo)
				if err != nil {
					CoordLog().Infof("update %v-%v schema to register failed: %v", ns, table, err)
				}
			}
		}
	}
}