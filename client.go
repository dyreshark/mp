package mp

import (
  "errors"
  "strconv"
  "io"
  "sync"
  "sync/atomic"
)

const (
  max64BitNumberCharsHex = 16 // 8 bytes * 2 chars per byte
)

type clientConnection struct {
  closed chan struct{}
  isClosed bool

  messages chan []byte
  currentMessage []byte
  readLock sync.Mutex

  seqNum uint64
  otherClient string
  connId string
  client *Client
  waitForConfirm bool
}

var (
  errWouldBlock = errors.New("Would block")
)

func newClientConnection(otherClient, connId string, client *Client) *clientConnection {
  return &clientConnection{
    // messages needs to remain unbuffered because of how our closed channel works.
    messages: make(chan []byte),
    closed: make(chan struct{}),
    otherClient: otherClient,
    connId: connId,
    client: client,
  }
}

func (c *clientConnection) readIntoBuffer(blocking bool) error {
  if c.currentMessage != nil {
    return nil
  }

  var msg []byte
  if !blocking {
    select {
    case msg = <-c.messages:
      break
    case <-c.closed:
      return io.EOF
    default:
      return errWouldBlock
    }
  } else {
    select {
    case msg = <-c.messages:
      break
    case <-c.closed:
      return io.EOF
    }
  }

  c.currentMessage = msg
  return nil
}

func (c *clientConnection) putNewMessage(msg *Message) bool {
  select {
  case <-c.closed:
    return false
  case c.messages <- msg.Data:
    return true
  }
}

func (c *clientConnection) Read(b []byte) (int, error) {
  c.readLock.Lock()
  defer c.readLock.Unlock()

  n := 0
  remSlice := b
  for n == 0 && len(remSlice) > 0 {
    block := n == 0
    err := c.readIntoBuffer(block)
    if err == errWouldBlock {
      return n, nil
    } else if err != nil {
      return n, err
    }

    x := copy(remSlice, c.currentMessage)
    if len(c.currentMessage) == x {
      c.currentMessage = nil
    } else {
      c.currentMessage = c.currentMessage[x:]
    }
    remSlice = remSlice[x:]
    n += x
  }

  return n, nil
}

func (c *clientConnection) Write(b []byte) (int, error) {
  if err := c.WriteMessage(b); err != nil {
    return 0, err
  }
  return len(b), nil
}

func (c *clientConnection) Close() error {
  c.isClosed = true
  close(c.closed)
  return nil
}

func (c *clientConnection) ReadMessage() ([]byte, error) {
  c.readLock.Lock()
  defer c.readLock.Unlock()

  err := c.readIntoBuffer(true)
  if err != nil {
    return nil, err
  }

  msg := c.currentMessage
  c.currentMessage = nil
  return msg, nil
}

func (c *clientConnection) nextSeqNum() uint64 {
  return atomic.AddUint64(&c.seqNum, 1)
}

func (c *clientConnection) WriteMessage(b []byte) error {
  msg := Message{
    Meta: MetaNone,
    SeqNum: c.nextSeqNum(),
    OtherClient: c.otherClient,
    Data: b,
  }

  err := c.client.sendMessage(&msg)
  if err != nil {
    return err
  }

  if c.waitForConfirm {
    panic("TODO")
  }

  return nil
}

func (c *clientConnection) OtherClient() string {
  return c.otherClient
}

func (c *clientConnection) SetWaitForConfirm(w bool) {
  c.waitForConfirm = true
}

func (c *clientConnection) WaitsForConfirm() bool {
  return c.waitForConfirm
}

type Client struct {
  name string
  server io.ReadWriteCloser
  serverRLock sync.Mutex
  serverWLock sync.Mutex

  translator MessageTranslator

  connections map[string]*clientConnection
  connectionsLock sync.Mutex

  waitingConnections map[string]*clientConnection
  waitingConnectionsLock sync.Mutex

  connNumber int64
  authed bool
}

func NewClient(
  name string,
  server io.ReadWriteCloser,
  translator MessageTranslator) *Client {

  return &Client{
    name: name,
    server: server,
    serverRLock: sync.Mutex{},
    serverWLock: sync.Mutex{},

    translator: translator,

    connections: make(map[string]*clientConnection),
    connectionsLock: sync.Mutex{},

    waitingConnections: make(map[string]*clientConnection),
    waitingConnectionsLock: sync.Mutex{},
  }
}

func (c *Client) sendMessage(m *Message) error {
  c.serverWLock.Lock()
  defer c.serverWLock.Unlock()

  return c.translator.WriteTo(c.server, m)
}

func (c *Client) recvMessage() (*Message, error) {
  c.serverRLock.Lock()
  defer c.serverRLock.Unlock()

  return c.translator.ReadFrom(c.server)
}

func (c *Client) Authenticate(password []byte) error {
  if c.authed {
    return errors.New("Multiple authentications requested")
  }

  msg := Message{
    Meta: MetaAuth,
    OtherClient: c.name,
    Data: password,
  }

  err := c.sendMessage(&msg)
  if err != nil {
    return err
  }

  resp, err := c.recvMessage()
  if err != nil {
    return err
  }

  if resp.Meta != MetaAuthOk {
    return errors.New(string(resp.Data))
  }

  return nil
}

func (c *Client) addConnection(conn *clientConnection) {
  c.connectionsLock.Lock()
  c.connections[conn.connId] = conn
  c.connectionsLock.Unlock()
}

func (c *Client) nextConnId() string {
  // In most cases there's no point in making garbage here.
  nextInt := atomic.AddInt64(&c.connNumber, 1)
  var microoptimization [64]byte
  buf := microoptimization[:]
  buf = append(buf, c.name...)
  buf = append(buf, ':')
  buf = strconv.AppendInt(buf, nextInt, 16)
  return string(buf)
}

func (c *Client) MakeConnection(otherClient, proto string) (Connection, error) {
  // Can I just use the machinery for sending a message using a Connection,
  // then wait until I either get an error or response?
  // Yes pls.
  id := c.nextConnId()
  conn := newClientConnection(otherClient, id, c)

  c.waitingConnectionsLock.Lock()
  c.waitingConnections[id] = conn
  c.waitingConnectionsLock.Unlock()

  msg := &Message{
    Meta: MetaConnSyn,
    OtherClient: otherClient,
    ConnectionId: id,
    Data: []byte(proto),
  }

  err := c.sendMessage(msg)
  if err != nil {
    return nil, err
  }

  // When we get our initial message, that means our connection has been moved from 
  // c.waitingConnections to c.connections for us. Hooray.
  data, err := conn.ReadMessage()
  if err != nil {
    return nil, err
  } else if data != nil {
    return nil, errors.New("Expected nil data ReadMessage()")
  }
  return conn, nil
}

func (c *Client) findConnection(id string) (*clientConnection, bool) {
  c.connectionsLock.Lock()
  a, b := c.connections[id]
  c.connectionsLock.Unlock()
  return a, b
}

// return (bool, error) is admittedly a bit weird. We'll update
// msg in-place with what we want to send back (if anything).
// Returns (?, err) on an unrecoverable error, (true, nil) if we
// want to respond, (false, nil) if we don't want to respond.
//
// Note that this function has free reign to update msg however it sees fit.
// Make a deep copy if you don't want it updated.
func (c *Client) handleMetaMessage(msg *Message) (resp bool, err error) {
  msg.Data = nil
  switch msg.Meta {
    case MetaNone:
      panic("Passed unmeta message to handleMetaMessage")
    case MetaAuth, MetaAuthOk, MetaAuthFailure:
      msg.Meta = MetaWAT
      return true, nil
    case MetaConnSyn:
      panic("TODO")
    case MetaConnAck:
      cid := msg.ConnectionId
      c.waitingConnectionsLock.Lock()
      conn, ok := c.waitingConnections[cid]
      if ok {
        delete(c.waitingConnections, cid)
      }
      c.waitingConnectionsLock.Unlock()

      if !ok {
        msg.Meta = MetaNoSuchConnection
        return true, nil
      }

      c.connectionsLock.Lock()
      c.connections[cid] = conn
      c.connectionsLock.Unlock()

      conn.putNewMessage(msg)
      return false, nil
    default:
      return false, errors.New("Unknown meta message type passed into handleMetaMessage")
  }
}

func (c *Client) Run() error {
  if !c.authed {
    return errors.New("Need to authenticate before running the client")
  }

  defer c.server.Close()
  for {
    msg, err := c.recvMessage()
    if err != nil {
      return err
    }

    if msg.Meta == MetaNone {
      conn, ok := c.findConnection(msg.ConnectionId)
      if !ok || !conn.putNewMessage(msg) {
        msg.Data = nil
        msg.Meta = MetaNoSuchConnection
        err = c.sendMessage(msg)
        if err != nil {
          return err
        }
      }
    } else {
      respond, err := c.handleMetaMessage(msg)
      if err != nil {
        return err
      }

      if respond {
        err = c.sendMessage(msg)
        if err != nil {
          return err
        }
      }
    }
  }
}