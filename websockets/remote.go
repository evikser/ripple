package websockets

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"sort"
	"time"

	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"github.com/kr-jaydeepp/ripple/data"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Time allowed to connect to server.
	dialTimeout = 5 * time.Second

	// time gap between reconnection
	connReconnectInterval = 30 * time.Second

	// server disconnect error message
	ServerDisconnectErrorMsg = "Client Error -1 ws: server disconnected"
)

type Remote struct {
	Incoming chan interface{}
	outgoing chan Syncer
	ws       *websocket.Conn
	url      *url.URL
	reConn   bool
	shutdown bool
}

// NewRemote returns a new remote session connected to the specified
// server endpoint URI. To close the connection, use Close().
func NewRemote(endpoint string, enableReconnection bool) (*Remote, error) {
	glog.Infoln(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", u.Host, dialTimeout)
	if err != nil {
		return nil, err
	}
	ws, _, err := websocket.NewClient(c, u, nil, 1024, 1024)
	if err != nil {
		return nil, err
	}
	r := &Remote{
		Incoming: make(chan interface{}, 1000),
		outgoing: make(chan Syncer, 10),
		ws:       ws,
		url:      u,
		reConn:   enableReconnection,
	}

	go r.run()
	return r, nil
}

// reConnect try to reconnect to server in case connection gets disconnected
func (r *Remote) reConnect() {
	glog.V(2).Info("reConnect!")
	ticker := time.NewTicker(connReconnectInterval)
	defer ticker.Stop()

connectLoop:
	for {
		select {
		case command, ok := <-r.outgoing:
			if !ok {
				glog.Errorln("outgoing channel closed")
				return
			}
			command.Fail("ws: server disconnected")

		// Time to reconnect
		case <-ticker.C:
			glog.Info("reConnect: Trying to reconnect")

			c, err := net.DialTimeout("tcp", r.url.Host, dialTimeout)
			if err != nil {
				glog.Error("reConnect: DailTimeout Error: ", err)
				continue
			}
			ws, _, err := websocket.NewClient(c, r.url, nil, 1024, 1024)
			if err != nil {
				glog.Error("reConnect: NewClient Error: ", err)
				continue
			}
			r.ws = ws
			go r.run()
			glog.Info("reConnect: successfull")
			break connectLoop
		}
	}
}

// Close shuts down the Remote session and blocks until all internal
// goroutines have been cleaned up.
// Any commands that are pending a response will return with an error.
func (r *Remote) Close() {
	glog.Info("closing remote connection")
	r.shutdown = true
	close(r.outgoing)

	// Drain the Incoming channel and block until it is closed,
	// indicating that this Remote is fully cleaned up.
	for _ = range r.Incoming {
	}
}

// run spawns the read/write pumps and then runs until Close() is called.
func (r *Remote) run() {
	outbound := make(chan interface{})
	inbound := make(chan []byte)
	pending := make(map[uint64]Syncer)
	timeout := make(chan uint64)
	timeoutCancellers := make(map[uint64]chan struct{})
	writePumpStopped := make(chan struct{})

	defer func() {
		close(outbound) // Shuts down the writePump

		// Cancel all pending commands with an error
		for _, c := range pending {
			c.Fail("ws: server disconnected")
		}

		// Drain the inbound channel and block until it is closed,
		// indicating that the readPump has returned.
		for range inbound {
		}

		if r.reConn && !r.shutdown {
			go r.reConnect()
		} else {
			close(r.Incoming)
		}
	}()

	// Spawn read/write goroutines
	go func() {
		defer r.ws.Close()
		defer close(writePumpStopped)
		r.writePump(outbound)
	}()
	go func() {
		defer close(inbound)
		r.readPump(inbound)
	}()

	commandTimeoutFunc := func(commandID uint64, timeoutCanceller chan struct{}) {
		timer := time.NewTimer(time.Minute)
		select {
		case <-timeoutCanceller:
			timer.Stop()
			return
		case <-timer.C:
			timeout <- commandID
			return
		}
	}

	// Main run loop
	var response Command
	for {
		select {
		case command, ok := <-r.outgoing:
			if !ok {
				return
			}

			// add the command to "pending" so that it doesn't get stuck if writepump has stopped
			id := reflect.ValueOf(command).Elem().FieldByName("Id").Uint()
			pending[id] = command

			// add cancellation before sending the command info
			canceller := make(chan struct{})
			timeoutCancellers[id] = canceller

			select {
			case <-writePumpStopped:
				delete(timeoutCancellers, id) // never actually sent the command
				return
			case outbound <- command:
				go commandTimeoutFunc(id, canceller)
			}

		case in, ok := <-inbound:
			if !ok {
				glog.Errorln("Connection closed by server")
				return
			}

			if err := json.Unmarshal(in, &response); err != nil {
				glog.Errorln(err.Error())
				continue
			}
			// Stream message
			factory, ok := streamMessageFactory[response.Type]
			if ok {
				cmd := factory()
				if err := json.Unmarshal(in, &cmd); err != nil {
					glog.Errorln(err.Error(), string(in))
					continue
				}
				r.Incoming <- cmd
				continue
			}

			// Command response message
			cmd, ok := pending[response.Id]
			if !ok {
				glog.Errorf("Unexpected message: %+v", response)
				continue
			}
			delete(pending, response.Id)
			if canceller, exists := timeoutCancellers[response.Id]; exists {
				canceller <- struct{}{}
				delete(timeoutCancellers, response.Id)
			}
			if err := json.Unmarshal(in, &cmd); err != nil {
				glog.Errorln(err.Error())
				cmd.Fail("error occured while unmarshalling")
				continue
			}
			cmd.Done()

		case id := <-timeout:
			if cmd, exists := pending[id]; exists {
				// this command has timed out
				delete(pending, id)
				cmd.Fail("command timed out")
				return
			}

			delete(timeoutCancellers, id)
		}
	}
}

// Synchronously get a single transaction
func (r *Remote) Tx(hash data.Hash256) (*TxResult, error) {
	cmd := &TxCommand{
		Command:     newCommand("tx"),
		Transaction: hash,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) accountTx(account data.Account, c chan *data.TransactionWithMetaData, pageSize int, minLedger, maxLedger int64) {
	defer close(c)
	cmd := newAccountTxCommand(account, pageSize, nil, minLedger, maxLedger)
	for ; ; cmd = newAccountTxCommand(account, pageSize, cmd.Result.Marker, minLedger, maxLedger) {
		r.outgoing <- cmd
		<-cmd.Ready
		if cmd.CommandError != nil {
			glog.Errorln(cmd.Error())
			return
		}
		for _, tx := range cmd.Result.Transactions {
			c <- tx
		}
		if cmd.Result.Marker == nil {
			return
		}
	}
}

// Retrieve all transactions for an account via
// https://ripple.com/build/rippled-apis/#account-tx. Will call
// `account_tx` multiple times, if a marker is returned.  Transactions
// are returned asynchonously to the channel returned by this
// function.
//
// Use minLedger -1 for the earliest ledger available.
// Use maxLedger -1 for the most recent validated ledger.
func (r *Remote) AccountTx(account data.Account, pageSize int, minLedger, maxLedger int64) chan *data.TransactionWithMetaData {
	c := make(chan *data.TransactionWithMetaData)
	go r.accountTx(account, c, pageSize, minLedger, maxLedger)
	return c
}

// Synchronously submit a single transaction
func (r *Remote) Submit(tx data.Transaction) (*SubmitResult, error) {
	_, raw, err := data.Raw(tx)
	if err != nil {
		return nil, err
	}
	cmd := &SubmitCommand{
		Command: newCommand("submit"),
		TxBlob:  fmt.Sprintf("%X", raw),
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously submit multiple transactions
func (r *Remote) SubmitBatch(txs []data.Transaction) ([]*SubmitResult, error) {
	commands := make([]*SubmitCommand, len(txs))
	results := make([]*SubmitResult, len(txs))
	for i := range txs {
		_, raw, err := data.Raw(txs[i])
		if err != nil {
			return nil, err
		}
		cmd := &SubmitCommand{
			Command: newCommand("submit"),
			TxBlob:  fmt.Sprintf("%X", raw),
		}
		r.outgoing <- cmd
		commands[i] = cmd
	}
	for i := range commands {
		<-commands[i].Ready
		results[i] = commands[i].Result
	}
	return results, nil
}

// Synchronously gets ledger entries
func (r *Remote) LedgerData(ledger interface{}, marker *data.Hash256) (*LedgerDataResult, error) {
	cmd := &LedgerDataCommand{
		Command: newCommand("ledger_data"),
		Ledger:  ledger,
		Marker:  marker,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) streamLedgerData(ledger interface{}, c chan data.LedgerEntrySlice) {
	defer close(c)
	cmd := newBinaryLedgerDataCommand(ledger, nil)
	for ; ; cmd = newBinaryLedgerDataCommand(ledger, cmd.Result.Marker) {
		r.outgoing <- cmd
		<-cmd.Ready
		if cmd.CommandError != nil {
			glog.Errorln(cmd.Error())
			return
		}
		les := make(data.LedgerEntrySlice, len(cmd.Result.State))
		for i, state := range cmd.Result.State {
			b, err := hex.DecodeString(state.Data + state.Index)
			if err != nil {
				glog.Errorln(cmd.Error())
				return
			}
			les[i], err = data.ReadLedgerEntry(bytes.NewReader(b), data.Hash256{})
			if err != nil {
				glog.Errorln(err.Error())
				glog.Errorln(state.Data)
				glog.Errorln(state.Index)
				continue
			}
		}
		c <- les
		if cmd.Result.Marker == nil {
			return
		}
	}
}

// Asynchronously retrieve all data for a ledger using the binary form
func (r *Remote) StreamLedgerData(ledger interface{}) chan data.LedgerEntrySlice {
	c := make(chan data.LedgerEntrySlice)
	go r.streamLedgerData(ledger, c)
	return c
}

// Synchronously gets a single ledger
func (r *Remote) Ledger(ledger interface{}, transactions bool) (*LedgerResult, error) {
	cmd := &LedgerCommand{
		Command:      newCommand("ledger"),
		Ledger:       ledger,
		Transactions: transactions,
		Expand:       true,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	cmd.Result.Ledger.Transactions.Sort()
	return cmd.Result, nil
}

func (r *Remote) LedgerHeader(ledger interface{}) (*LedgerHeaderResult, error) {
	cmd := &LedgerHeaderCommand{
		Command: newCommand("ledger_header"),
		Ledger:  ledger,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests paths
func (r *Remote) RipplePathFind(src, dest data.Account, amount data.Amount, srcCurr *[]data.Currency) (*RipplePathFindResult, error) {
	cmd := &RipplePathFindCommand{
		Command:       newCommand("ripple_path_find"),
		SrcAccount:    src,
		SrcCurrencies: srcCurr,
		DestAccount:   dest,
		DestAmount:    amount,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests account info
func (r *Remote) AccountInfo(a data.Account) (*AccountInfoResult, error) {
	cmd := &AccountInfoCommand{
		Command: newCommand("account_info"),
		Account: a,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously requests account line info
func (r *Remote) AccountLines(account data.Account, ledgerIndex interface{}) (*AccountLinesResult, error) {
	var (
		lines  data.AccountLineSlice
		marker *data.Hash256
	)
	for {
		cmd := &AccountLinesCommand{
			Command:     newCommand("account_lines"),
			Account:     account,
			Limit:       400,
			Marker:      marker,
			LedgerIndex: ledgerIndex,
		}
		r.outgoing <- cmd
		<-cmd.Ready
		switch {
		case cmd.CommandError != nil:
			return nil, cmd.CommandError
		case cmd.Result.Marker != nil:
			lines = append(lines, cmd.Result.Lines...)
			marker = cmd.Result.Marker
			if cmd.Result.LedgerSequence != nil {
				ledgerIndex = *cmd.Result.LedgerSequence
			}
		default:
			cmd.Result.Lines = append(lines, cmd.Result.Lines...)
			cmd.Result.Lines.SortByCurrencyAmount()
			return cmd.Result, nil
		}
	}
}

// Synchronously requests account offers
func (r *Remote) AccountOffers(account data.Account, ledgerIndex interface{}) (*AccountOffersResult, error) {
	var (
		offers data.AccountOfferSlice
		marker *data.Hash256
	)
	for {
		cmd := &AccountOffersCommand{
			Command:     newCommand("account_offers"),
			Account:     account,
			Limit:       400,
			Marker:      marker,
			LedgerIndex: ledgerIndex,
		}
		r.outgoing <- cmd
		<-cmd.Ready
		switch {
		case cmd.CommandError != nil:
			return nil, cmd.CommandError
		case cmd.Result.Marker != nil:
			offers = append(offers, cmd.Result.Offers...)
			marker = cmd.Result.Marker
			if cmd.Result.LedgerSequence != nil {
				ledgerIndex = *cmd.Result.LedgerSequence
			}
		default:
			cmd.Result.Offers = append(offers, cmd.Result.Offers...)
			sort.Sort(cmd.Result.Offers)
			return cmd.Result, nil
		}
	}
}

func (r *Remote) BookOffers(taker data.Account, ledgerIndex interface{}, pays, gets data.Asset) (*BookOffersResult, error) {
	cmd := &BookOffersCommand{
		Command:     newCommand("book_offers"),
		LedgerIndex: ledgerIndex,
		Taker:       taker,
		TakerPays:   pays,
		TakerGets:   gets,
		Limit:       5000, // Marker not implemented....
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// Synchronously subscribe to streams and receive a confirmation message
// Streams are recived asynchronously over the Incoming channel
func (r *Remote) Subscribe(ledger, transactions, transactionsProposed, server bool) (*SubscribeResult, error) {
	streams := []string{}
	if ledger {
		streams = append(streams, "ledger")
	}
	if transactions {
		streams = append(streams, "transactions")
	}
	if transactionsProposed {
		streams = append(streams, "transactions_proposed")
	}
	if server {
		streams = append(streams, "server")
	}
	cmd := &SubscribeCommand{
		Command: newCommand("subscribe"),
		Streams: streams,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}

	if ledger && cmd.Result.LedgerStreamMsg == nil {
		return nil, fmt.Errorf("Missing ledger subscribe response")
	}
	if server && cmd.Result.ServerStreamMsg == nil {
		return nil, fmt.Errorf("Missing server subscribe response")
	}
	return cmd.Result, nil
}

type OrderBookSubscription struct {
	TakerGets data.Asset `json:"taker_gets"`
	TakerPays data.Asset `json:"taker_pays"`
	Snapshot  bool       `json:"snapshot"`
	Both      bool       `json:"both"`
}

func (r *Remote) SubscribeOrderBooks(books []OrderBookSubscription) (*SubscribeResult, error) {
	cmd := &SubscribeCommand{
		Command: newCommand("subscribe"),
		Streams: []string{"ledger", "server"},
		Books:   books,
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

func (r *Remote) Fee() (*FeeResult, error) {
	cmd := &FeeCommand{
		Command: newCommand("fee"),
	}
	r.outgoing <- cmd
	<-cmd.Ready
	if cmd.CommandError != nil {
		return nil, cmd.CommandError
	}
	return cmd.Result, nil
}

// readPump reads from the websocket and sends to inbound channel.
// Expects to receive PONGs at specified interval, or logs an error and returns.
func (r *Remote) readPump(inbound chan<- []byte) {
	r.ws.SetReadDeadline(time.Now().Add(pongWait))
	r.ws.SetPongHandler(func(string) error { r.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := r.ws.ReadMessage()
		if err != nil {
			glog.Errorln(err)
			return
		}
		glog.V(2).Infoln(dump(message))
		r.ws.SetReadDeadline(time.Now().Add(pongWait))
		inbound <- message
	}
}

// Consumes from the outbound channel and sends them over the websocket.
// Also sends PING messages at the specified interval.
// Returns when outbound channel is closed, or an error is encountered.
func (r *Remote) writePump(outbound <-chan interface{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {

		// An outbound message is available to send
		case message, ok := <-outbound:
			if !ok {
				r.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			b, err := json.Marshal(message)
			if err != nil {
				// Outbound message cannot be JSON serialized (log it and continue)
				glog.Errorln(err)
				continue
			}

			glog.V(2).Infoln(dump(b))
			if err := r.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				glog.Errorln(err)
				return
			}

		// Time to send a ping
		case <-ticker.C:
			if err := r.ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				glog.Errorln(err)
				return
			}
		}
	}
}

func dump(b []byte) string {
	var v map[string]interface{}
	json.Unmarshal(b, &v)
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out)
}
