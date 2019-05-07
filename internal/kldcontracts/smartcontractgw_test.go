// Copyright 2019 Kaleido

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kldcontracts

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/go-openapi/spec"
	"github.com/julienschmidt/httprouter"
	"github.com/kaleido-io/ethconnect/internal/kldbind"
	"github.com/kaleido-io/ethconnect/internal/kldevents"
	"github.com/kaleido-io/ethconnect/internal/kldmessages"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

const (
	simpleStorage = "pragma solidity >=0.4.22 <0.6.0;\n\n/** @title simplestorage */\n\ncontract simplestorage {\nuint public storedData;\n\nconstructor(uint initVal) public {\nstoredData = initVal;\n}\n\nfunction set(uint x) public {\nstoredData = x;\n}\n\nfunction get() public view returns (uint retVal) {\nreturn storedData;\n}\n}"
)

func TestCobraInitContractGateway(t *testing.T) {
	assert := assert.New(t)
	cmd := cobra.Command{}
	conf := &SmartContractGatewayConf{}
	CobraInitContractGateway(&cmd, conf)
	assert.NotNil(cmd.Flag("openapi-path"))
	assert.NotNil(cmd.Flag("openapi-baseurl"))
}

func TestNewSmartContractGatewayBadURL(t *testing.T) {
	NewSmartContractGateway(
		&SmartContractGatewayConf{
			BaseURL: " :",
		},
		nil, nil, nil,
	)
}

func TestNewSmartContractGatewayWithEvents(t *testing.T) {
	dir := tempdir()
	defer cleanup(dir)
	assert := assert.New(t)
	s, err := NewSmartContractGateway(
		&SmartContractGatewayConf{
			BaseURL: "http://localhost/api/v1",
			SubscriptionManagerConf: kldevents.SubscriptionManagerConf{
				EventLevelDBPath: path.Join(dir, "db"),
			},
		},
		nil, nil, nil,
	)
	assert.NoError(err)
	assert.NotNil(s.(*smartContractGW).submgr)
}

func TestNewSmartContractGatewayWithEventsFail(t *testing.T) {
	dir := tempdir()
	defer cleanup(dir)
	assert := assert.New(t)
	dbpath := path.Join(dir, "db")
	ioutil.WriteFile(dbpath, []byte("not a database"), 0644)
	_, err := NewSmartContractGateway(
		&SmartContractGatewayConf{
			BaseURL: "http://localhost/api/v1",
			SubscriptionManagerConf: kldevents.SubscriptionManagerConf{
				EventLevelDBPath: dbpath,
			},
		},
		nil, nil, nil,
	)
	assert.Regexp("Event-stream subscription manager", err.Error())
}

func TestPreDeployCompileAndPostDeploy(t *testing.T) {
	// writes real files and tests end to end
	assert := assert.New(t)
	msg := kldmessages.DeployContract{
		Solidity: simpleStorage,
	}
	msg.Headers.ID = "message1"
	dir := tempdir()
	defer cleanup(dir)

	scgw, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
			BaseURL:     "http://localhost/api/v1",
		},
		nil, nil, nil,
	)

	err := scgw.PreDeploy(&msg)
	assert.NoError(err)

	id := msg.Headers.ID
	assert.Equal("message1", id)
	assert.Empty(msg.Solidity)
	assert.NotEmpty(msg.Compiled)
	assert.NotEmpty(msg.DevDoc)

	deployStashBytes, err := ioutil.ReadFile(path.Join(dir, "abi_message1.deploy.json"))
	assert.NoError(err)
	var deployStash kldmessages.DeployContract
	err = json.Unmarshal(deployStashBytes, &deployStash)
	assert.NoError(err)
	assert.NotEmpty(deployStash.CompilerVersion)

	contractAddr := common.HexToAddress("0x0123456789AbcdeF0123456789abCdef01234567")
	receipt := kldmessages.TransactionReceipt{
		ReplyCommon: kldmessages.ReplyCommon{
			Headers: kldmessages.ReplyHeaders{
				CommonHeaders: kldmessages.CommonHeaders{
					ID: "message2",
				},
				ReqID: "message1",
			},
		},
		ContractAddress: &contractAddr,
	}
	err = scgw.PostDeploy(&receipt)
	assert.NoError(err)

	swaggerBytes, err := ioutil.ReadFile(path.Join(dir, "contract_0123456789abcdef0123456789abcdef01234567.swagger.json"))
	assert.NoError(err)
	assert.Equal("2.0", gjson.Get(string(swaggerBytes), "swagger").String())
	assert.Equal("localhost", gjson.Get(string(swaggerBytes), "host").String())
	assert.Equal("simplestorage", gjson.Get(string(swaggerBytes), "info.title").String())
	assert.Equal("message1", gjson.Get(string(swaggerBytes), "info.x-kaleido-deployment-id").String())
	assert.Equal("/api/v1/contracts/0123456789abcdef0123456789abcdef01234567", gjson.Get(string(swaggerBytes), "basePath").String())

	abi, err := scgw.(*smartContractGW).loadABIForInstance("0123456789abcdef0123456789abcdef01234567")
	assert.NoError(err)
	assert.Equal("set", abi.ABI.Methods["set"].Name)

	// Check we can list it back over REST
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	req := httptest.NewRequest("GET", "/contracts", bytes.NewReader([]byte{}))
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	assert.Equal(200, res.Result().StatusCode)
	var body []*contractInfo
	err = json.NewDecoder(res.Body).Decode(&body)
	assert.NoError(err)
	assert.Equal(1, len(body))
	assert.Equal("simplestorage", body[0].Name)
	assert.Equal("0123456789abcdef0123456789abcdef01234567", body[0].Address)

	// Check we can get it back over REST
	req = httptest.NewRequest("GET", "/contracts/0123456789abcdef0123456789abcdef01234567", bytes.NewReader([]byte{}))
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	assert.Equal(200, res.Result().StatusCode)
	var info contractInfo
	err = json.NewDecoder(res.Body).Decode(&info)
	assert.NoError(err)
	assert.Equal("simplestorage", info.Name)
	assert.Equal("0123456789abcdef0123456789abcdef01234567", info.Address)

	// Check we can get the full swagger back over REST
	req = httptest.NewRequest("GET", "/contracts/0123456789abcdef0123456789abcdef01234567?swagger", bytes.NewReader([]byte{}))
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	assert.Equal(200, res.Result().StatusCode)
	var swagger spec.Swagger
	err = json.NewDecoder(res.Body).Decode(&swagger)
	assert.NoError(err)
	assert.Equal("simplestorage", swagger.Info.Title)

	// Check we can get the full swagger back over REST for download with a default from
	req = httptest.NewRequest("GET", "/contracts/0123456789abcdef0123456789abcdef01234567?swagger&from=0x0123456789abcdef0123456789abcdef01234567&download", bytes.NewReader([]byte{}))
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	assert.Equal(200, res.Result().StatusCode)
	err = json.NewDecoder(res.Body).Decode(&swagger)
	assert.NoError(err)
	assert.Equal("simplestorage", swagger.Info.Title)
	assert.Equal("attachment; filename=\"0123456789abcdef0123456789abcdef01234567.swagger.json\"", res.HeaderMap.Get("Content-Disposition"))
	assert.Equal("0x0123456789abcdef0123456789abcdef01234567", swagger.Parameters["fromParam"].SimpleSchema.Default)
}

func TestLoadABIFailure(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	ioutil.WriteFile(path.Join(dir, "contract_addr1.abi.json"), []byte(":bad json"), 0644)
	_, err := scgw.loadABIForInstance("addr1")
	assert.Regexp("Failed to load installed ABI for contract address", err.Error())
}

func TestLoadDeployMsgOK(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	goodMsg := &kldmessages.DeployContract{}
	deployBytes, _ := json.Marshal(goodMsg)
	ioutil.WriteFile(path.Join(dir, "abi_abi1.deploy.json"), deployBytes, 0644)
	_, err := scgw.loadDeployMsgForFactory("abi1")
	assert.NoError(err)
}

func TestLoadDeployMsgMissing(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	_, err := scgw.loadDeployMsgForFactory("abi1")
	assert.Regexp("Failed to find ABI with ID abi1:", err.Error())
}

func TestLoadDeployMsgFailure(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	ioutil.WriteFile(path.Join(dir, "abi_abi1.deploy.json"), []byte(":bad json"), 0644)
	_, err := scgw.loadDeployMsgForFactory("abi1")
	assert.Regexp("Failed to load ABI with ID abi1", err.Error())
}

func TestPreDeployCompileFailure(t *testing.T) {
	assert := assert.New(t)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: "/anypath",
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	msg := &kldmessages.DeployContract{
		Solidity: "bad solidity",
	}
	err := scgw.PreDeploy(msg)
	assert.Regexp("Solidity compilation failed", err.Error())
}

func TestPreDeployMsgWrite(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badpath"),
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	msg := &kldmessages.DeployContract{
		Solidity: simpleStorage,
	}

	err := scgw.writeAbiInfo("request1", msg)
	assert.Regexp("Failed to write deployment details", err.Error())
}

func TestPostDeployOpenFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badpath"),
			BaseURL:     "http://localhost/api/v1",
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	replyMsg := &kldmessages.TransactionReceipt{
		ReplyCommon: kldmessages.ReplyCommon{
			Headers: kldmessages.ReplyHeaders{
				ReqID: "message1",
			},
		},
	}

	err := scgw.PostDeploy(replyMsg)
	assert.Regexp("Unable to recover pre-deploy message", err.Error())
}

func TestPostDeployDecodeFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	replyMsg := &kldmessages.TransactionReceipt{
		ReplyCommon: kldmessages.ReplyCommon{
			Headers: kldmessages.ReplyHeaders{
				ReqID: "message1",
			},
		},
	}
	ioutil.WriteFile(path.Join(dir, "message1.deploy.json"), []byte("invalid json"), 0664)

	err := scgw.PostDeploy(replyMsg)
	assert.Regexp("Unable to recover pre-deploy message", err.Error())
}

func TestPostDeploySwaggerGenFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	_, err := scgw.genSwagger("request1", "", nil, "", "")
	assert.Regexp("ABI cannot be nil", err.Error())
}

func TestGenSwaggerWriteFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badpath"),
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	a := kldbind.ABI{
		ABI: abi.ABI{},
	}
	_, err := scgw.genSwagger("req1", "", &a, "", "0123456789AbcdeF0123456789abCdef0123456")
	assert.Regexp("Failed to write OpenAPI JSON", err.Error())
}

func TestStoreABIWriteFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badpath"),
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	a := kldbind.ABI{
		ABI: abi.ABI{},
	}
	err := scgw.storeABI("req1", "0123456789AbcdeF0123456789abCdef0123456", &a)
	assert.Regexp("Failed to write ABI JSON", err.Error())
}

func TestLoadABIReadFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badpath"),
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	_, err := scgw.loadABIForInstance("invalid")
	assert.Regexp("Failed to find installed ABI for contract address", err.Error())
}

func TestLoadABIBadData(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)
	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	ioutil.WriteFile(path.Join(dir, "badness.abi.json"), []byte(":not json"), 0644)
	_, err := scgw.loadABIForInstance("badness")
	assert.Regexp("Failed to find installed ABI for contract address", err.Error())
}

func TestBuildIndex(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	var emptySwagger spec.Swagger
	swaggerBytes, _ := json.Marshal(&emptySwagger)
	ioutil.WriteFile(path.Join(dir, "contract_0123456789abcdef0123456789abcdef01234567.swagger.json"), swaggerBytes, 0644)
	okSwagger := spec.Swagger{
		SwaggerProps: spec.SwaggerProps{
			Info: &spec.Info{
				InfoProps: spec.InfoProps{
					Title: "good one",
				},
			},
		},
	}
	swaggerBytes, _ = json.Marshal(&okSwagger)
	ioutil.WriteFile(path.Join(dir, "contract_123456789abcdef0123456789abcdef012345678.swagger.json"), swaggerBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "contract_23456789abcdef0123456789abcdef0123456789.swagger.json"), swaggerBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "contract_3456789abcdef0123456789abcdef01234567890.swagger.json"), []byte(":bad swagger"), 0644)
	ioutil.WriteFile(path.Join(dir, "abi_840b629f-2e46-413b-9671-553a886ca7bb.swagger.json"), swaggerBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "abi_e27be4cf-6ae2-411e-8088-db2992618938.swagger.json"), swaggerBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "abi_519526b2-0879-41f4-93c0-09acaa62e2da.swagger.json"), []byte(":bad swagger"), 0644)

	deployMsg := &kldmessages.DeployContract{
		ContractName: "abideployable",
	}
	deployBytes, _ := json.Marshal(&deployMsg)
	ioutil.WriteFile(path.Join(dir, "abi_840b629f-2e46-413b-9671-553a886ca7bb.deploy.json"), deployBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "abi_e27be4cf-6ae2-411e-8088-db2992618938.deploy.json"), deployBytes, 0644)
	ioutil.WriteFile(path.Join(dir, "abi_519526b2-0879-41f4-93c0-09acaa62e2da.deploy.json"), []byte(":bad json"), 0644)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	assert.Equal(2, len(scgw.contractIndex))
	info := scgw.contractIndex["123456789abcdef0123456789abcdef012345678"].(*contractInfo)
	assert.Equal("good one", info.Name)
	assert.Equal("good one", info.Name)

	req := httptest.NewRequest("GET", "/contracts", bytes.NewReader([]byte{}))
	res := httptest.NewRecorder()
	params := httprouter.Params{}
	scgw.listContractsOrABIs(res, req, params)
	assert.Equal(200, res.Result().StatusCode)
	var contractInfos []*contractInfo
	err := json.NewDecoder(res.Body).Decode(&contractInfos)
	assert.NoError(err)
	assert.Equal(2, len(contractInfos))
	assert.Equal("123456789abcdef0123456789abcdef012345678", contractInfos[0].Address)
	assert.Equal("23456789abcdef0123456789abcdef0123456789", contractInfos[1].Address)

	req = httptest.NewRequest("GET", "/abis", bytes.NewReader([]byte{}))
	res = httptest.NewRecorder()
	params = httprouter.Params{}
	scgw.listContractsOrABIs(res, req, params)
	assert.Equal(200, res.Result().StatusCode)
	var abiInfos []*abiInfo
	err = json.NewDecoder(res.Body).Decode(&abiInfos)
	assert.NoError(err)
	assert.Equal(2, len(abiInfos))
	assert.Equal("840b629f-2e46-413b-9671-553a886ca7bb", abiInfos[0].ID)
	assert.Equal("e27be4cf-6ae2-411e-8088-db2992618938", abiInfos[1].ID)
}

func TestAddFileToSwaggerIndexOpenFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	scgw.addFileToContractIndex("", path.Join(dir, "baddir", "0123456789abcdef0123456789abcdef01234567.swagger.json"), time.Now())
	assert.Equal(0, len(scgw.contractIndex))
}

func TestGetContractOrABIFail(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	scgw.contractIndex["123456789abcdef0123456789abcdef012345678"] = &contractInfo{
		Name:    "zombie",
		Address: "123456789abcdef0123456789abcdef012345678",
	}

	// One that exists in the index, but for some reason the file isn't there - should be a 500
	req := httptest.NewRequest("GET", "/contracts/123456789abcdef0123456789abcdef012345678?openapi", bytes.NewReader([]byte{}))
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)
	assert.Equal(500, res.Result().StatusCode)

	// One that simply doesn't exist in the index - should be a 404
	req = httptest.NewRequest("GET", "/abis/23456789abcdef0123456789abcdef0123456789?openapi", bytes.NewReader([]byte{}))
	res = httptest.NewRecorder()
	router = &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)
	assert.Equal(404, res.Result().StatusCode)
}

func TestGetContractUI(t *testing.T) {
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	scgw.contractIndex["123456789abcdef0123456789abcdef012345678"] = &contractInfo{
		Name:    "any",
		Address: "123456789abcdef0123456789abcdef012345678",
	}

	req := httptest.NewRequest("GET", "/contracts/123456789abcdef0123456789abcdef012345678?ui", bytes.NewReader([]byte{}))
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)
	assert.Equal(200, res.Result().StatusCode)
	body, _ := ioutil.ReadAll(res.Body)
	assert.Regexp("Ethconnect REST Gateway", string(body))
}

func TestAddABISingleSolidity(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("files", "simplestorage.sol")
	part.Write([]byte(simpleStorage))
	writer.Close()

	req := httptest.NewRequest("POST", "/abis", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(200, res.Result().StatusCode)
	info := &abiInfo{}
	err := json.NewDecoder(res.Body).Decode(info)
	assert.NoError(err)
	assert.Equal("simplestorage", info.Name)
}

func TestAddABIZipNested(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("files", "solfiles.zip")
	zipWriter := zip.NewWriter(part)
	solWriter, _ := zipWriter.Create("solfiles/simplestorage.sol")
	solWriter.Write([]byte(simpleStorage))
	zipWriter.Close()
	writer.WriteField("source", "solfiles/simplestorage.sol")
	writer.Close()

	req := httptest.NewRequest("POST", "/abis", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(200, res.Result().StatusCode)
	info := &abiInfo{}
	err := json.NewDecoder(res.Body).Decode(info)
	assert.NoError(err)
	assert.Equal("simplestorage", info.Name)
}

func TestAddABIStoreFail(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: path.Join(dir, "badness"),
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("files", "simplestorage.sol")
	part.Write([]byte(simpleStorage))
	writer.Close()

	req := httptest.NewRequest("POST", "/abis", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(500, res.Result().StatusCode)
	errInfo := &restErrMsg{}
	err := json.NewDecoder(res.Body).Decode(errInfo)
	assert.NoError(err)
	assert.Regexp("Failed to write OpenAPI JSON", errInfo.Message)
}

func TestAddABIBadZip(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("files", "simplestorage.zip")
	part.Write([]byte(simpleStorage))
	writer.Close()

	req := httptest.NewRequest("POST", "/abis", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(400, res.Result().StatusCode)
	errInfo := &restErrMsg{}
	err := json.NewDecoder(res.Body).Decode(errInfo)
	assert.NoError(err)
	assert.Regexp("Error unarchiving supplied zip file", errInfo.Message)
}

func TestAddABIZipNestedNoSource(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("files", "solfiles.zip")
	zipWriter := zip.NewWriter(part)
	solWriter, _ := zipWriter.Create("solfiles/simplestorage.sol")
	solWriter.Write([]byte(simpleStorage))
	zipWriter.Close()
	writer.Close()

	req := httptest.NewRequest("POST", "/abis", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(400, res.Result().StatusCode)
	errInfo := &restErrMsg{}
	err := json.NewDecoder(res.Body).Decode(errInfo)
	assert.NoError(err)
	assert.Equal("Failed to compile solidity: No .sol files found in root. Please set a 'source' query param or form field to the relative path of your solidity", errInfo.Message)
}

func TestAddABIZiNotMultipart(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	req := httptest.NewRequest("POST", "/abis", bytes.NewReader([]byte{}))
	res := httptest.NewRecorder()
	router := &httprouter.Router{}
	scgw.AddRoutes(router)
	router.ServeHTTP(res, req)

	assert.Equal(400, res.Result().StatusCode)
	errInfo := &restErrMsg{}
	err := json.NewDecoder(res.Body).Decode(errInfo)
	assert.NoError(err)
	assert.Equal("Could not parse supplied multi-part form data: request Content-Type isn't multipart/form-data", errInfo.Message)
}

func TestCompileMultipartFormSolidityBadDir(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	_, err := scgw.compileMultipartFormSolidity(path.Join(dir, "baddir"), nil)
	assert.EqualError(err, "Failed to read extracted multi-part form data")
}

func TestCompileMultipartFormSolidityBadSolc(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)
	os.Setenv("KLD_SOLC_0_99", "badness")

	ioutil.WriteFile(path.Join(dir, "solidity.sol"), []byte(simpleStorage), 0644)
	req := httptest.NewRequest("POST", "/abis?compiler=0.99", bytes.NewReader([]byte{}))
	_, err := scgw.compileMultipartFormSolidity(dir, req)
	assert.EqualError(err, "Failed checking solc version")
	os.Unsetenv("KLD_SOLC_0_99")
}

func TestCompileMultipartFormSolidityBadCompilerVerReq(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	ioutil.WriteFile(path.Join(dir, "solidity.sol"), []byte(simpleStorage), 0644)
	req := httptest.NewRequest("POST", "/abis?compiler=0.99", bytes.NewReader([]byte{}))
	_, err := scgw.compileMultipartFormSolidity(dir, req)
	assert.EqualError(err, "Could not find a configured compiler for requested Solidity major version 0.99")
}

func TestCompileMultipartFormSolidityBadSolidity(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	ioutil.WriteFile(path.Join(dir, "solidity.sol"), []byte("this is not the solidity you are looking for"), 0644)
	req := httptest.NewRequest("POST", "/abis", bytes.NewReader([]byte{}))
	_, err := scgw.compileMultipartFormSolidity(dir, req)
	assert.Regexp("Failed to compile", err.Error())
}

func TestExtractMultiPartFileBadFile(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	err := scgw.extractMultiPartFile(dir, &multipart.FileHeader{
		Filename: "/stuff.zip",
	})
	assert.EqualError(err, "Filenames cannot contain slashes. Use a zip file to upload a directory structure")
}

func TestExtractMultiPartFileBadInput(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	err := scgw.extractMultiPartFile(dir, &multipart.FileHeader{
		Filename: "stuff.zip",
	})
	assert.EqualError(err, "Failed to read archive")
}

func TestStoreDeployableABIMissingABI(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	assert := assert.New(t)
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	_, err := scgw.storeDeployableABI(&kldmessages.DeployContract{}, nil)
	assert.EqualError(err, "Must supply ABI to install an existing ABI into the REST Gateway")
}

func TestAddFileToABIIndexBadFileSwallowsError(t *testing.T) {
	dir := tempdir()
	defer cleanup(dir)

	s, _ := NewSmartContractGateway(
		&SmartContractGatewayConf{
			StoragePath: dir,
		},
		nil, nil, nil,
	)
	scgw := s.(*smartContractGW)

	scgw.addFileToABIIndex("", "badness", time.Now().UTC())
}
