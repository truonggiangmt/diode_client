// Diode Network Client
// Copyright 2019-2021 IoT Blockchain Technology Corporation LLC (IBTC)
// Licensed under the Diode License, Version 1.0

// Package rpc ConnectedPort has been turned into an actor
// https://www.gophercon.co.uk/videos/2016/an-actor-model-in-go/
// Ensure all accesses are wrapped in port.cmdChan <- func() { ... }

package rpc

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/diodechain/diode_client/blockquick"
	"github.com/diodechain/diode_client/config"
	"github.com/diodechain/diode_client/contract"
	"github.com/diodechain/diode_client/db"
	"github.com/diodechain/diode_client/edge"
	"github.com/diodechain/diode_client/util"
	"github.com/diodechain/openssl"
	"github.com/diodechain/zap"
	"github.com/dominicletz/genserver"
)

const (
	// 4194304 = 1024 * 4096 (server limit is 41943040)
	packetLimit   = 65000
	ticketBound   = 4194304
	callQueueSize = 1024
)

var (
	globalRequestID          uint64 = 0
	errEmptyBNSresult               = fmt.Errorf("couldn't resolve name (null)")
	errSendTransactionFailed        = fmt.Errorf("server returned false")
	errClientClosed                 = fmt.Errorf("rpc client was closed")
	errPortOpenTimeout              = fmt.Errorf("portopen timeout")
)

// Client struct for rpc client
type Client struct {
	host                  string
	backoff               Backoff
	s                     *SSL
	enableMetrics         bool
	metrics               *Metrics
	Verbose               bool
	cm                    *callManager
	blockTicker           *time.Ticker
	blockTickerDuration   time.Duration
	finishBlockTickerChan chan bool
	localTimeout          time.Duration
	wg                    sync.WaitGroup
	pool                  *DataPool
	config                *config.Config
	bq                    *blockquick.Window
	Latency               int64
	onConnect             func(util.Address)
	// close event
	OnClose func()

	isClosed bool
	srv      *genserver.GenServer
}

func getRequestID() uint64 {
	return atomic.AddUint64(&globalRequestID, 1)
}

// NewClient returns rpc client
func NewClient(host string, cfg *config.Config, pool *DataPool) *Client {
	client := &Client{
		host:                  host,
		srv:                   genserver.New("Client"),
		cm:                    NewCallManager(callQueueSize),
		finishBlockTickerChan: make(chan bool, 1),
		blockTickerDuration:   15 * time.Second,
		localTimeout:          100 * time.Millisecond,
		pool:                  pool,
		backoff: Backoff{
			Min:    5 * time.Second,
			Max:    10 * time.Second,
			Factor: 2,
			Jitter: true,
		},
		config:        cfg,
		enableMetrics: cfg.EnableMetrics,
	}

	if client.enableMetrics {
		client.metrics = NewMetrics()
	}

	if !config.AppConfig.LogDateTime {
		client.srv.DeadlockCallback = nil
	}

	return client
}

func (client *Client) doConnect() (err error) {
	err = client.doDial()
	if err != nil {
		client.Error("Failed to connect: (%v)", err)
		// Retry to connect
		isOk := false
		for i := 1; i <= client.config.RetryTimes; i++ {
			dur := client.backoff.Duration()
			client.Info("Retry to connect (%d/%d), waiting %s", i, client.config.RetryTimes, dur.String())
			time.Sleep(dur)
			err = client.doDial()
			if err == nil {
				isOk = true
				break
			}
			if client.config.Debug {
				client.Debug("Failed to connect: (%v)", err)
			}
		}
		if !isOk {
			return fmt.Errorf("failed to connect to host: %s", client.host)
		}
	}
	// enable keepalive
	if client.config.EnableKeepAlive {
		err = client.s.EnableKeepAlive()
		if err != nil {
			return err
		}
		err = client.s.SetKeepAliveInterval(client.config.KeepAliveInterval)
		if err != nil {
			return err
		}
	}
	return err
}

func (client *Client) doDial() (err error) {
	start := time.Now()
	client.s, err = DialContext(initSSLCtx(client.config), client.host, openssl.InsecureSkipHostVerification)
	client.Latency = time.Since(start).Milliseconds()
	return
}

// Info logs to logger in Info level
func (client *Client) Info(msg string, args ...interface{}) {
	client.config.Logger.ZapLogger().Info(fmt.Sprintf(msg, args...), zap.String("server", client.host))
}

// Debug logs to logger in Debug level
func (client *Client) Debug(msg string, args ...interface{}) {
	client.config.Logger.ZapLogger().Debug(fmt.Sprintf(msg, args...), zap.String("server", client.host))
}

// Error logs to logger in Error level
func (client *Client) Error(msg string, args ...interface{}) {
	client.config.Logger.ZapLogger().Error(fmt.Sprintf(msg, args...), zap.String("server", client.host))
}

// Warn logs to logger in Warn level
func (client *Client) Warn(msg string, args ...interface{}) {
	client.config.Logger.ZapLogger().Warn(fmt.Sprintf(msg, args...), zap.String("server", client.host))
}

// Crit logs to logger in Crit level
func (client *Client) Crit(msg string, args ...interface{}) {
	client.config.Logger.ZapLogger().Fatal(fmt.Sprintf(msg, args...), zap.String("server", client.host))
}

// Host returns the non-resolved addr name of the host
func (client *Client) Host() (host string) {
	client.call(func() { host = client.s.addr })
	return
}

// GetServerID returns server address
func (client *Client) GetServerID() (serverID [20]byte, err error) {
	client.call(func() {
		serverID, err = client.s.GetServerID()
		if err != nil {
			serverID = util.EmptyAddress
		}
	})
	return
}

// GetDeviceKey returns device key of given ref
func (client *Client) GetDeviceKey(ref string) string {
	prefixByt, err := client.GetServerID()
	if err != nil {
		return ""
	}
	prefix := util.EncodeToString(prefixByt[:])
	return fmt.Sprintf("%s:%s", prefix, ref)
}

func (client *Client) waitResponse(call *Call) (res interface{}, err error) {
	defer call.Clean(CLOSED)
	defer client.srv.Cast(func() { client.cm.RemoveCallByID(call.id) })
	resp, ok := <-call.response
	if !ok {
		err = CancelledError{client.Host()}
		if call.sender != nil {
			call.sender.sendErr = io.EOF
			call.sender.Close()
		}
		return
	}
	if rpcError, ok := resp.(edge.Error); ok {
		err = RPCError{rpcError}
		if call.sender != nil {
			call.sender.sendErr = RPCError{rpcError}
			call.sender.Close()
		}
		return
	}
	res = resp
	return res, nil
}

// RespondContext sends a message (a response) without expecting a response
func (client *Client) RespondContext(requestID uint64, responseType string, method string, args ...interface{}) (call *Call, err error) {
	buf := &bytes.Buffer{}
	_, err = edge.NewResponseMessage(buf, requestID, responseType, method, args...)
	if err != nil {
		return
	}
	call = &Call{
		sender: nil,
		id:     requestID,
		method: method,
		data:   buf,
	}
	err = client.insertCall(call)
	return
}

func (client *Client) call(fun func()) {
	client.srv.Call(fun)
}

// CastContext returns a response future after calling the rpc
func (client *Client) CastContext(sender *ConnectedPort, method string, args ...interface{}) (call *Call, err error) {
	var parseCallback func([]byte) (interface{}, error)
	buf := &bytes.Buffer{}
	reqID := getRequestID()
	parseCallback, err = edge.NewMessage(buf, reqID, method, args...)
	if err != nil {
		return
	}
	call = &Call{
		sender:   sender,
		id:       reqID,
		method:   method,
		data:     buf,
		Parse:    parseCallback,
		response: make(chan interface{}),
	}
	err = client.insertCall(call)
	return
}

func (client *Client) insertCall(call *Call) (err error) {
	client.call(func() {
		if client.isClosed {
			err = errClientClosed
			return
		}
		err = client.cm.Insert(call)
	})
	return
}

// CallContext returns the response after calling the rpc
func (client *Client) CallContext(method string, parse func(buffer []byte) (interface{}, error), args ...interface{}) (res interface{}, err error) {
	var resCall *Call
	var ts time.Time
	var tsDiff time.Duration
	resCall, err = client.CastContext(nil, method, args...)
	if err != nil {
		return
	}
	ts = time.Now()
	res, err = client.waitResponse(resCall)
	if err != nil {
		switch err.(type) {
		case CancelledError:
			// client.Warn("Call %s has been cancelled, drop the call", method)
			return
		}
	}
	tsDiff = time.Since(ts)
	if client.enableMetrics {
		client.metrics.UpdateRPCTimer(tsDiff)
	}
	client.Debug("Got response: %s [%v]", method, tsDiff)
	return
}

// CheckTicket should client send traffic ticket to server
func (client *Client) CheckTicket() (err error) {
	var checked bool
	client.call(func() {
		counter := client.s.Counter()
		checked = client.s.TotalBytes() > counter+ticketBound
	})
	if checked {
		err = client.SubmitNewTicket()
	}
	return
}

// ValidateNetwork validate blockchain network is secure and valid
// Run blockquick algorithm, more information see: https://eprint.iacr.org/2019/579.pdf
func (client *Client) validateNetwork() error {

	lvbn, lvbh := restoreLastValid()
	blockNumMin := lvbn - windowSize + 1

	// Fetching at least window size blocks -- this should be cached on disk instead.
	blockHeaders, err := client.GetBlockHeadersUnsafe(blockNumMin, lvbn)
	if err != nil {
		client.Error("Cannot fetch blocks %v-%v error: %v", blockNumMin, lvbn, err)
		return err
	}
	if len(blockHeaders) != windowSize {
		client.Error("ValidateNetwork(): len(blockHeaders) != windowSize (%v, %v)", len(blockHeaders), windowSize)
		return fmt.Errorf("validateNetwork(): len(blockHeaders) != windowSize (%v, %v)", len(blockHeaders), windowSize)
	}

	// Checking last valid header
	hash := blockHeaders[windowSize-1].Hash()
	if hash != lvbh {
		// the lvbh was different, remove the lvbn
		if client.Verbose {
			client.Error("DEBUG: Reference block does not match -- resetting lvbn.")
		}
		db.DB.Del(lvbnKey)
		return fmt.Errorf("sent reference block does not match %v: %v != %v", lvbn, lvbh, hash)
	}

	// Checking chain of previous blocks
	for i := windowSize - 2; i >= 0; i-- {
		if blockHeaders[i].Hash() != blockHeaders[i+1].Parent() {
			return fmt.Errorf("recevied blocks parent is not his parent: %+v %+v", blockHeaders[i+1], blockHeaders[i])
		}
		if !blockHeaders[i].ValidateSig() {
			return fmt.Errorf("recevied blocks signature is not valid: %v", blockHeaders[i])
		}
	}

	// Starting to fetch new blocks
	peak, err := client.GetBlockPeak()
	if err != nil {
		return err
	}
	blockNumMax := peak - confirmationSize + 1
	// fetch more blocks than windowSize
	blocks, err := client.GetBlockquick(uint64(lvbn), uint64(windowSize+confirmationSize+1))
	if err != nil {
		return err
	}

	win, err := blockquick.New(blockHeaders, windowSize)
	if err != nil {
		return err
	}

	for _, block := range blocks {
		// due to blocks order by block number, break loop here
		if block.Number() > blockNumMax {
			break
		}
		err := win.AddBlock(block, true)
		if err != nil {
			return err
		}
	}

	newlvbn, _ := win.Last()
	if newlvbn == lvbn {
		if peak-windowSize > lvbn {
			return fmt.Errorf("couldn't validate any new blocks %v < %v", lvbn, peak)
		}
	}

	client.call(func() { client.bq = win })
	client.storeLastValid()
	return nil
}

/**
 * Server RPC
 */

// GetBlockPeak returns block peak
func (client *Client) GetBlockPeak() (uint64, error) {
	rawBlockPeak, err := client.CallContext("getblockpeak", nil)
	if err != nil {
		return 0, err
	}
	if blockPeak, ok := rawBlockPeak.(uint64); ok {
		return blockPeak, nil
	}
	return 0, nil
}

// GetBlockquick returns block headers used for blockquick algorithm
func (client *Client) GetBlockquick(lastValid uint64, windowSize uint64) ([]blockquick.BlockHeader, error) {
	rawSequence, err := client.CallContext("getblockquick2", nil, lastValid, windowSize)
	if err != nil {
		return nil, err
	}
	if sequence, ok := rawSequence.([]uint64); ok {
		return client.GetBlockHeadersUnsafe2(sequence)
	}
	return nil, nil
}

// GetBlockHeaderUnsafe returns an unchecked block header from the server
func (client *Client) GetBlockHeaderUnsafe(blockNum uint64) (bh blockquick.BlockHeader, err error) {
	var rawHeader interface{}
	rawHeader, err = client.CallContext("getblockheader2", nil, blockNum)
	if err != nil {
		return
	}
	if blockHeader, ok := rawHeader.(blockquick.BlockHeader); ok {
		bh = blockHeader
		return
	}
	return
}

// GetBlockHeadersUnsafe2 returns a range of block headers
// TODO: use copy instead reference of BlockHeader
func (client *Client) GetBlockHeadersUnsafe2(blockNumbers []uint64) ([]blockquick.BlockHeader, error) {
	count := len(blockNumbers)
	headersCount := 0
	responses := make(map[uint64]blockquick.BlockHeader, count)
	mx := sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(count)
	for _, i := range blockNumbers {
		go func(bn uint64) {
			defer wg.Done()
			header, err := client.GetBlockHeaderUnsafe(bn)
			if err != nil {
				return
			}
			mx.Lock()
			headersCount++
			responses[bn] = header
			mx.Unlock()
		}(i)
	}
	wg.Wait()

	if headersCount != count {
		return []blockquick.BlockHeader{}, fmt.Errorf("failed fetching all blocks")
	}

	// copy responses to headers
	headers := make([]blockquick.BlockHeader, headersCount)
	for i, bn := range blockNumbers {
		if bh, ok := responses[bn]; ok {
			headers[i] = bh
		}
	}
	return headers, nil
}

// GetBlockHeaderValid returns a validated recent block header
// (only available for the last windowsSize blocks)
func (client *Client) GetBlockHeaderValid(blockNum uint64) blockquick.BlockHeader {
	// client.rm.Lock()
	// defer client.rm.Unlock()
	return client.bq.GetBlockHeader(blockNum)
}

// GetBlockHeadersUnsafe returns a consecutive range of block headers
func (client *Client) GetBlockHeadersUnsafe(blockNumMin uint64, blockNumMax uint64) ([]blockquick.BlockHeader, error) {
	if blockNumMin > blockNumMax {
		return nil, fmt.Errorf("GetBlockHeadersUnsafe(): blockNumMin needs to be <= max")
	}
	count := blockNumMax - blockNumMin + 1
	blockNumbers := make([]uint64, 0, count)
	for i := blockNumMin; i <= blockNumMax; i++ {
		blockNumbers = append(blockNumbers, uint64(i))
	}
	return client.GetBlockHeadersUnsafe2(blockNumbers)
}

// GetBlock returns block
// TODO: make sure this rpc works (disconnect from server)
func (client *Client) GetBlock(blockNum uint64) (interface{}, error) {
	return client.CallContext("getblock", nil, blockNum)
}

// GetObject returns network object for device
func (client *Client) GetObject(deviceID [20]byte) (*edge.DeviceTicket, error) {
	if len(deviceID) != 20 {
		return nil, fmt.Errorf("device ID must be 20 bytes")
	}
	// encDeviceID := util.EncodeToString(deviceID[:])
	rawObject, err := client.CallContext("getobject", nil, deviceID[:])
	if err != nil {
		return nil, err
	}
	if device, ok := rawObject.(*edge.DeviceTicket); ok {
		device.BlockHash, err = client.ResolveBlockHash(device.BlockNumber)
		return device, err
	}
	return nil, nil
}

// GetNode returns network address for node
func (client *Client) GetNode(nodeID [20]byte) (*edge.ServerObj, error) {
	rawNode, err := client.CallContext("getnode", nil, nodeID[:])
	if err != nil {
		return nil, err
	}
	if obj, ok := rawNode.(*edge.ServerObj); ok {
		return obj, nil
	}
	return nil, fmt.Errorf("GetNode(): parseerror")
}

// Greet Initiates the connection
// TODO: test compression flag
func (client *Client) greet() error {
	_, err := client.CastContext(nil, "hello", uint64(1000))
	if err != nil {
		return err
	}
	return client.SubmitNewTicket()
}

func (client *Client) SubmitNewTicket() (err error) {
	if client.bq == nil {
		return
	}

	var ticket *edge.DeviceTicket
	ticket, err = client.newTicket()
	if err != nil {
		return
	}
	err = client.submitTicket(ticket)
	return
}

// SignTransaction return signed transaction
func (client *Client) SignTransaction(tx *edge.Transaction) (err error) {
	var privKey *ecdsa.PrivateKey
	client.call(func() {
		privKey, err = client.s.GetClientPrivateKey()
	})
	if err != nil {
		return err
	}
	return tx.Sign(privKey)
}

// NewTicket returns ticket
func (client *Client) newTicket() (*edge.DeviceTicket, error) {
	serverID, err := client.s.GetServerID()
	if err != nil {
		return nil, err
	}
	client.s.UpdateCounter(client.s.TotalBytes())
	lvbn, lvbh := client.LastValid()
	client.Debug("New ticket: %d", lvbn)
	ticket := &edge.DeviceTicket{
		ServerID:         serverID,
		BlockNumber:      lvbn,
		BlockHash:        lvbh[:],
		FleetAddr:        client.config.FleetAddr,
		TotalConnections: client.s.TotalConnections(),
		TotalBytes:       client.s.TotalBytes(),
		LocalAddr:        []byte(client.s.LocalAddr().String()),
	}
	if err := ticket.ValidateValues(); err != nil {
		return nil, err
	}
	privKey, err := client.s.GetClientPrivateKey()
	if err != nil {
		return nil, err
	}
	err = ticket.Sign(privKey)
	if err != nil {
		return nil, err
	}
	if !ticket.ValidateDeviceSig(client.config.ClientAddr) {
		return nil, fmt.Errorf("ticket not verifiable")
	}

	return ticket, nil
}

// SubmitTicket submit ticket to server
// TODO: resend when got too old error
func (client *Client) submitTicket(ticket *edge.DeviceTicket) error {
	resp, err := client.CallContext("ticket", nil, uint64(ticket.BlockNumber), ticket.FleetAddr[:], uint64(ticket.TotalConnections), uint64(ticket.TotalBytes), ticket.LocalAddr, ticket.DeviceSig)
	if err != nil {
		return fmt.Errorf("failed to submit ticket: %v", err)
	}
	if lastTicket, ok := resp.(edge.DeviceTicket); ok {
		if lastTicket.Err == edge.ErrTicketTooLow {
			sid, _ := client.s.GetServerID()
			lastTicket.ServerID = sid
			lastTicket.FleetAddr = client.config.FleetAddr

			if !lastTicket.ValidateDeviceSig(client.config.ClientAddr) {
				lastTicket.LocalAddr = util.DecodeForce(lastTicket.LocalAddr)
			}
			if lastTicket.ValidateDeviceSig(client.config.ClientAddr) {
				client.s.totalBytes = lastTicket.TotalBytes + 1024
				client.s.totalConnections = lastTicket.TotalConnections + 1
				err = client.SubmitNewTicket()
				if err != nil {
					return fmt.Errorf("failed to re-submit ticket: %v", err)
				}
			} else {
				client.Warn("received fake ticket.. last_ticket=%v", lastTicket)
			}
		} else if lastTicket.Err == edge.ErrTicketTooOld {
			client.Info("received too old ticket")
		}
		return nil
	}
	return err
}

// PortOpen call portopen RPC
func (client *Client) PortOpen(deviceID [20]byte, port string, mode string) (*edge.PortOpen, error) {
	rawPortOpen, err := client.CallContext("portopen", nil, deviceID[:], port, mode)
	if err != nil {
		// if error string is 4 bytes string, it's the timeout error from server
		if len(err.Error()) == 4 {
			err = errPortOpenTimeout
		}
		return nil, err
	}
	if portOpen, ok := rawPortOpen.(*edge.PortOpen); ok {
		return portOpen, nil
	}
	return nil, nil
}

// ResponsePortOpen response portopen request
func (client *Client) ResponsePortOpen(portOpen *edge.PortOpen, err error) error {
	if err != nil {
		_, err = client.RespondContext(portOpen.RequestID, "error", "portopen", portOpen.Ref, err.Error())
	} else {
		_, err = client.RespondContext(portOpen.RequestID, "response", "portopen", portOpen.Ref, "ok")
	}
	if err != nil {
		return err
	}
	return nil
}

// CastPortClose cast portclose RPC
func (client *Client) CastPortClose(ref string) (err error) {
	_, err = client.CastContext(nil, "portclose", ref)
	return err
}

// PortClose portclose RPC
func (client *Client) PortClose(ref string) (interface{}, error) {
	return client.CallContext("portclose", nil, ref)
}

// Ping call ping RPC
func (client *Client) Ping() (interface{}, error) {
	return client.CallContext("ping", nil)
}

// SendTransaction send signed transaction to server
func (client *Client) SendTransaction(tx *edge.Transaction) (result bool, err error) {
	var encodedRLPTx []byte
	var res interface{}
	var ok bool
	err = client.SignTransaction(tx)
	if err != nil {
		return
	}
	encodedRLPTx, err = tx.ToRLP()
	if err != nil {
		return
	}
	res, err = client.CallContext("sendtransaction", nil, encodedRLPTx)
	if res, ok = res.(string); ok {
		result = res == "ok"
		if !result {
			err = errSendTransactionFailed
		}
		return
	}
	return
}

// GetAccount returns account information: nonce, balance, storage root, code
func (client *Client) GetAccount(blockNumber uint64, account [20]byte) (*edge.Account, error) {
	rawAccount, err := client.CallContext("getaccount", nil, blockNumber, account[:])
	if err != nil {
		return nil, err
	}
	if account, ok := rawAccount.(*edge.Account); ok {
		return account, nil
	}
	return nil, nil
}

// GetStateRoots returns state roots
func (client *Client) GetStateRoots(blockNumber uint64) (*edge.StateRoots, error) {
	rawStateRoots, err := client.CallContext("getstateroots", nil, blockNumber)
	if err != nil {
		return nil, err
	}
	if stateRoots, ok := rawStateRoots.(*edge.StateRoots); ok {
		return stateRoots, nil
	}
	return nil, nil
}

// GetValidAccount returns valid account information: nonce, balance, storage root, code
func (client *Client) GetValidAccount(blockNumber uint64, account [20]byte) (*edge.Account, error) {
	if blockNumber <= 0 {
		bn, _ := client.LastValid()
		blockNumber = uint64(bn)
	}
	act, err := client.GetAccount(blockNumber, account)
	if err != nil {
		return nil, err
	}
	sts, err := client.GetStateRoots(blockNumber)
	if err != nil {
		return nil, err
	}
	if uint64(sts.Find(act.StateRoot())) == act.StateTree().Modulo {
		return act, nil
	}
	return nil, nil
}

// GetAccountNonce returns the nonce of the given account, or 0
func (client *Client) GetAccountNonce(blockNumber uint64, account [20]byte) uint64 {
	act, _ := client.GetValidAccount(blockNumber, account)
	if act == nil {
		return 0
	}
	return uint64(act.Nonce)
}

// GetAccountValue returns account storage value
func (client *Client) GetAccountValue(blockNumber uint64, account [20]byte, rawKey []byte) (*edge.AccountValue, error) {
	if blockNumber <= 0 {
		bn, _ := client.LastValid()
		blockNumber = uint64(bn)
	}
	// pad key to 32 bytes
	key := util.PaddingBytesPrefix(rawKey, 0, 32)
	rawAccountValue, err := client.CallContext("getaccountvalue", nil, blockNumber, account[:], key)
	if err != nil {
		return nil, err
	}
	if accountValue, ok := rawAccountValue.(*edge.AccountValue); ok {
		return accountValue, nil
	}
	return nil, nil
}

// GetAccountValueInt returns account value as Integer
func (client *Client) GetAccountValueInt(blockNumber uint64, addr [20]byte, key []byte) big.Int {
	raw, err := client.GetAccountValueRaw(blockNumber, addr, key)
	var ret big.Int
	if err != nil {
		return ret
	}
	ret.SetBytes(raw)
	return ret
}

// GetAccountValueRaw returns account value
func (client *Client) GetAccountValueRaw(blockNumber uint64, addr [20]byte, key []byte) ([]byte, error) {
	if blockNumber <= 0 {
		bn, _ := client.LastValid()
		blockNumber = uint64(bn)
	}
	acv, err := client.GetAccountValue(blockNumber, addr, key)
	if err != nil {
		return NullData, err
	}
	// get account roots
	acr, err := client.GetAccountRoots(blockNumber, addr)
	if err != nil {
		return NullData, err
	}
	acvTree := acv.AccountTree()
	// Verify the calculated proof value matches the specific known root
	if acr.Find(acv.AccountRoot()) != int(acvTree.Modulo) {
		client.config.Logger.Error("Received wrong merkle proof %v != %v", acr.Find(acv.AccountRoot()), int(acvTree.Modulo))
		// fmt.Printf("key := %#v\n", key)
		// fmt.Printf("roots := %#v\n", acr)
		// fmt.Printf("rawTestTree := %#v\n", acvTree.RawTree)
		return NullData, fmt.Errorf("wrong merkle proof")
	}
	raw, err := acvTree.Get(key)
	if err != nil {
		return NullData, err
	}
	return raw, nil
}

// GetAccountRoots returns account state roots
func (client *Client) GetAccountRoots(blockNumber uint64, account [20]byte) (*edge.AccountRoots, error) {
	if blockNumber <= 0 {
		bn, _ := client.LastValid()
		blockNumber = uint64(bn)
	}
	rawAccountRoots, err := client.CallContext("getaccountroots", nil, blockNumber, account[:])
	if err != nil {
		return nil, err
	}
	if accountRoots, ok := rawAccountRoots.(*edge.AccountRoots); ok {
		return accountRoots, nil
	}
	return nil, nil
}

// ResolveReverseBNS resolves the (primary) destination of the BNS entry
func (client *Client) ResolveReverseBNS(addr Address) (name string, err error) {
	key := contract.BNSReverseEntryLocation(addr)
	raw, err := client.GetAccountValueRaw(0, contract.BNSAddr, key)
	if err != nil {
		return name, err
	}

	size := binary.BigEndian.Uint16(raw[len(raw)-2:])
	if size%2 == 0 {
		size = size / 2
		return string(raw[:size]), nil
	}
	// Todo fetch additional string parts
	return string(raw[:30]), nil
}

// ResolveBNS resolves the (primary) destination of the BNS entry
func (client *Client) ResolveBNS(name string) (addr []Address, err error) {
	client.Info("Resolving BNS: %s", name)
	arrayKey := contract.BNSDestinationArrayLocation(name)
	size := client.GetAccountValueInt(0, contract.BNSAddr, arrayKey)

	// Fallback for old style DNS entries
	intSize := size.Int64()

	// Todo remove once memory issue is found
	if intSize > 128 {
		client.Error("Read invalid BNS entry count: %d", intSize)
		intSize = 0
	}

	if intSize == 0 {
		key := contract.BNSEntryLocation(name)
		raw, err := client.GetAccountValueRaw(0, contract.BNSAddr, key)
		if err != nil {
			return addr, err
		}

		addr = make([]util.Address, 1)
		copy(addr[0][:], raw[12:])
		if addr[0] == [20]byte{} {
			return addr, errEmptyBNSresult
		}
		return addr, nil
	}

	for i := int64(0); i < intSize; i++ {
		key := contract.BNSDestinationArrayElementLocation(name, int(i))
		raw, err := client.GetAccountValueRaw(0, contract.BNSAddr, key)
		if err != nil {
			client.Error("Read invalid BNS record offset: %d %v (%v)", i, err, string(raw))
			continue
		}

		var address util.Address
		copy(address[:], raw[12:])
		addr = append(addr, address)
	}
	if len(addr) == 0 {
		return addr, errEmptyBNSresult
	}
	return addr, nil
}

// ResolveBNSOwner resolves the owner of the BNS entry
func (client *Client) ResolveBNSOwner(name string) (addr Address, err error) {
	key := contract.BNSOwnerLocation(name)
	raw, err := client.GetAccountValueRaw(0, contract.BNSAddr, key)
	if err != nil {
		return [20]byte{}, err
	}

	copy(addr[:], raw[12:])
	if addr == [20]byte{} {
		return [20]byte{}, errEmptyBNSresult
	}
	return addr, nil
}

// ResolveBlockHash resolves a missing blockhash by blocknumber
func (client *Client) ResolveBlockHash(blockNumber uint64) (blockHash []byte, err error) {
	if blockNumber == 0 {
		return
	}
	blockHeader := client.bq.GetBlockHeader(blockNumber)
	if blockHeader.Number() == 0 {
		lvbn, _ := client.bq.Last()
		client.Info("Validating ticket based on non-checked block %v %v", blockNumber, lvbn)
		blockHeader, err = client.GetBlockHeaderUnsafe(blockNumber)
		if err != nil {
			return
		}
	}
	hash := blockHeader.Hash()
	blockHash = hash[:]
	return
}

// IsDeviceAllowlisted returns is given address allowlisted
func (client *Client) IsDeviceAllowlisted(fleetAddr Address, clientAddr Address) bool {
	if fleetAddr == config.DefaultFleetAddr {
		return true
	}
	key := contract.DeviceAllowlistKey(clientAddr)
	num := client.GetAccountValueInt(0, fleetAddr, key)

	return num.Int64() == 1
}

// Closed returns whether client had closed
func (client *Client) Closed() bool {
	return client.isClosed
}

// Close rpc client
func (client *Client) Close() {
	doCleanup := true
	client.call(func() {
		if client.isClosed {
			doCleanup = false
			return
		}
		client.isClosed = true
		// remove existing calls
		client.cm.RemoveCalls()
		if client.blockTicker != nil {
			client.blockTicker.Stop()
		}
		client.finishBlockTickerChan <- true
		if client.OnClose != nil {
			client.OnClose()
		}
		client.s.Close()
	})
	if doCleanup {
		// remove open ports
		client.pool.ClosePorts(client)
		client.srv.Shutdown(0)
	}
}

// Start process rpc inbound message and outbound message
func (client *Client) Start() {
	client.srv.Cast(func() {
		if err := client.doStart(); err != nil {
			if !client.isClosed {
				client.Warn("Client connect failed: %v", err)
			}
			client.srv.Shutdown(0)
		}
	})

	go func() {
		if err := client.initialize(); err != nil {
			if !client.isClosed {
				client.Warn("Client start failed: %v", err)
				client.Close()
			}
		}
	}()
}

func (client *Client) doStart() (err error) {
	if err = client.doConnect(); err != nil {
		return
	}
	client.addWorker(client.recvMessage)
	client.addWorker(client.watchLatestBlock)
	client.cm.SendCallPtr = client.sendCall
	return
}

func (client *Client) initialize() (err error) {
	err = client.validateNetwork()
	if err != nil && strings.Contains(err.Error(), "sent reference block does not match") {
		// the lvbn was removed, we can validate network again
		err = client.validateNetwork()
	}
	if err != nil {
		return
	}

	var serverID [20]byte
	serverID, err = client.s.GetServerID()
	if err != nil {
		err = fmt.Errorf("failed to get server id: %v", err)
		return
	}
	err = client.greet()
	if err != nil {
		return fmt.Errorf("failed to submitTicket to server: %v", err)
	}
	if client.onConnect != nil {
		client.onConnect(serverID)
	}
	return
}
