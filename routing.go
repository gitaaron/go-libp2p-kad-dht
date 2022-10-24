package dht

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
  "strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"

	"github.com/ipfs/go-cid"
	u "github.com/ipfs/go-ipfs-util"
	"github.com/libp2p/go-libp2p-kad-dht/internal"
	internalConfig "github.com/libp2p/go-libp2p-kad-dht/internal/config"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	kb "github.com/libp2p/go-libp2p-kbucket"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/multiformats/go-multihash"
)

// This file implements the Routing interface for the IpfsDHT struct.

// Global vars for experiment
var activeTesting map[string]bool
var activeTestingLock sync.RWMutex

// Basic Put/Get

// PutValue adds value corresponding to given Key.
// This is the top level "Store" operation of the DHT
func (dht *IpfsDHT) PutValue(ctx context.Context, key string, value []byte, opts ...routing.Option) (err error) {
	if !dht.enableValues {
		return routing.ErrNotSupported
	}

	logger.Debugw("putting value", "key", internal.LoggableRecordKeyString(key))

	// don't even allow local users to put bad values.
	if err := dht.Validator.Validate(key, value); err != nil {
		return err
	}

	old, err := dht.getLocal(ctx, key)
	if err != nil {
		// Means something is wrong with the datastore.
		return err
	}

	// Check if we have an old value that's not the same as the new one.
	if old != nil && !bytes.Equal(old.GetValue(), value) {
		// Check to see if the new one is better.
		i, err := dht.Validator.Select(key, [][]byte{value, old.GetValue()})
		if err != nil {
			return err
		}
		if i != 0 {
			return fmt.Errorf("can't replace a newer value with an older value")
		}
	}

	rec := record.MakePutRecord(key, value)
	rec.TimeReceived = u.FormatRFC3339(time.Now())
	err = dht.putLocal(ctx, key, rec)
	if err != nil {
		return err
	}

	peers, err := dht.GetClosestPeers(ctx, key)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for _, p := range peers {
		wg.Add(1)
		go func(p peer.ID) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer wg.Done()
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.Value,
				ID:   p,
			})

			err := dht.protoMessenger.PutValue(ctx, p, rec)
			if err != nil {
				logger.Debugf("failed putting value to peer: %s", err)
			}
		}(p)
	}
	wg.Wait()

	return nil
}

// recvdVal stores a value and the peer from which we got the value.
type recvdVal struct {
	Val  []byte
	From peer.ID
}

// GetValue searches for the value corresponding to given Key.
func (dht *IpfsDHT) GetValue(ctx context.Context, key string, opts ...routing.Option) (_ []byte, err error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	// apply defaultQuorum if relevant
	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}
	opts = append(opts, Quorum(internalConfig.GetQuorum(&cfg)))

	responses, err := dht.SearchValue(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	var best []byte

	for r := range responses {
		best = r
	}

	if ctx.Err() != nil {
		return best, ctx.Err()
	}

	if best == nil {
		return nil, routing.ErrNotFound
	}
	logger.Debugf("GetValue %v %x", internal.LoggableRecordKeyString(key), best)
	return best, nil
}

// SearchValue searches for the value corresponding to given Key and streams the results.
func (dht *IpfsDHT) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}

	responsesNeeded := 0
	if !cfg.Offline {
		responsesNeeded = internalConfig.GetQuorum(&cfg)
	}

	stopCh := make(chan struct{})
	valCh, lookupRes := dht.getValues(ctx, key, stopCh)

	out := make(chan []byte)
	go func() {
		defer close(out)
		best, peersWithBest, aborted := dht.searchValueQuorum(ctx, key, valCh, stopCh, out, responsesNeeded)
		if best == nil || aborted {
			return
		}

		updatePeers := make([]peer.ID, 0, dht.bucketSize)
		select {
		case l := <-lookupRes:
			if l == nil {
				return
			}

			for _, p := range l.peers {
				if _, ok := peersWithBest[p]; !ok {
					updatePeers = append(updatePeers, p)
				}
			}
		case <-ctx.Done():
			return
		}

		dht.updatePeerValues(dht.Context(), key, best, updatePeers)
	}()

	return out, nil
}

func (dht *IpfsDHT) searchValueQuorum(ctx context.Context, key string, valCh <-chan recvdVal, stopCh chan struct{},
	out chan<- []byte, nvals int) ([]byte, map[peer.ID]struct{}, bool) {
	numResponses := 0
	return dht.processValues(ctx, key, valCh,
		func(ctx context.Context, v recvdVal, better bool) bool {
			numResponses++
			if better {
				select {
				case out <- v.Val:
				case <-ctx.Done():
					return false
				}
			}

			if nvals > 0 && numResponses > nvals {
				close(stopCh)
				return true
			}
			return false
		})
}

func (dht *IpfsDHT) processValues(ctx context.Context, key string, vals <-chan recvdVal,
	newVal func(ctx context.Context, v recvdVal, better bool) bool) (best []byte, peersWithBest map[peer.ID]struct{}, aborted bool) {
loop:
	for {
		if aborted {
			return
		}

		select {
		case v, ok := <-vals:
			if !ok {
				break loop
			}

			// Select best value
			if best != nil {
				if bytes.Equal(best, v.Val) {
					peersWithBest[v.From] = struct{}{}
					aborted = newVal(ctx, v, false)
					continue
				}
				sel, err := dht.Validator.Select(key, [][]byte{best, v.Val})
				if err != nil {
					logger.Warnw("failed to select best value", "key", internal.LoggableRecordKeyString(key), "error", err)
					continue
				}
				if sel != 1 {
					aborted = newVal(ctx, v, false)
					continue
				}
			}
			peersWithBest = make(map[peer.ID]struct{})
			peersWithBest[v.From] = struct{}{}
			best = v.Val
			aborted = newVal(ctx, v, true)
		case <-ctx.Done():
			return
		}
	}

	return
}

func (dht *IpfsDHT) updatePeerValues(ctx context.Context, key string, val []byte, peers []peer.ID) {
	fixupRec := record.MakePutRecord(key, val)
	for _, p := range peers {
		go func(p peer.ID) {
			//TODO: Is this possible?
			if p == dht.self {
				err := dht.putLocal(ctx, key, fixupRec)
				if err != nil {
					logger.Error("Error correcting local dht entry:", err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second*30)
			defer cancel()
			err := dht.protoMessenger.PutValue(ctx, p, fixupRec)
			if err != nil {
				logger.Debug("Error correcting DHT entry: ", err)
			}
		}(p)
	}
}

func (dht *IpfsDHT) getValues(ctx context.Context, key string, stopQuery chan struct{}) (<-chan recvdVal, <-chan *lookupWithFollowupResult) {
	valCh := make(chan recvdVal, 1)
	lookupResCh := make(chan *lookupWithFollowupResult, 1)

	logger.Debugw("finding value", "key", internal.LoggableRecordKeyString(key))

	if rec, err := dht.getLocal(ctx, key); rec != nil && err == nil {
		select {
		case valCh <- recvdVal{
			Val:  rec.GetValue(),
			From: dht.self,
		}:
		case <-ctx.Done():
		}
	}

	go func() {
		defer close(valCh)
		defer close(lookupResCh)
		lookupRes, err := dht.runLookupWithFollowup(ctx, key,
			func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type: routing.SendingQuery,
					ID:   p,
				})

				rec, peers, err := dht.protoMessenger.GetValue(ctx, p, key)
				if err != nil {
					return nil, err
				}

				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type:      routing.PeerResponse,
					ID:        p,
					Responses: peers,
				})

				if rec == nil {
					return peers, nil
				}

				val := rec.GetValue()
				if val == nil {
					logger.Debug("received a nil record value")
					return peers, nil
				}
				if err := dht.Validator.Validate(key, val); err != nil {
					// make sure record is valid
					logger.Debugw("received invalid record (discarded)", "error", err)
					return peers, nil
				}

				// the record is present and valid, send it out for processing
				select {
				case valCh <- recvdVal{
					Val:  val,
					From: p,
				}:
				case <-ctx.Done():
					return nil, ctx.Err()
				}

				return peers, nil
			},
			func() bool {
				select {
				case <-stopQuery:
					return true
				default:
					return false
				}
			},
		)

		if err != nil {
			return
		}
		lookupResCh <- lookupRes

		if ctx.Err() == nil {
			dht.refreshRTIfNoShortcut(kb.ConvertKey(key), lookupRes)
		}
	}()

	return valCh, lookupResCh
}

func (dht *IpfsDHT) refreshRTIfNoShortcut(key kb.ID, lookupRes *lookupWithFollowupResult) {
	if lookupRes.completed {
		// refresh the cpl for this key as the query was successful
		dht.routingTable.ResetCplRefreshedAtForID(key, time.Now())
	}
}

// Provider abstraction for indirect stores.
// Some DHTs store values directly, while an indirect store stores pointers to
// locations of the value, similarly to Coral and Mainline DHT.

// Provide makes this node announce that it can provide a value for the given key
func (dht *IpfsDHT) Provide(ctx context.Context, key cid.Cid, brdcst bool) (err error) {
	if !dht.enableProviders {
		return routing.ErrNotSupported
	} else if !key.Defined() {
		return fmt.Errorf("invalid cid: undefined")
	}
	// Check if this provide is called after as part of an experiment
	log := false
	ipfsTestFolder := os.Getenv("PERFORMANCE_TEST_DIR")
	if ipfsTestFolder == "" {
		ipfsTestFolder = "/ipfs-tests"
	}
	if _, err := os.Stat(path.Join(ipfsTestFolder, fmt.Sprintf("provide-%v", key.String()))); err == nil {
		os.Remove(path.Join(ipfsTestFolder, fmt.Sprintf("provide-%v", key.String())))
		log = true
		activeTestingLock.Lock()
		if activeTesting == nil {
			activeTesting = map[string]bool{}
		}
		_, ok := activeTesting[key.String()]
		if ok {
			// There is an active testing on.
			activeTestingLock.Unlock()
			return nil
		} else {
			activeTesting[key.String()] = true
		}
		activeTestingLock.Unlock()
	}

	keyMH := key.Hash()
	logger.Debugw("providing", "cid", key, "mh", internal.LoggableProviderRecordBytes(keyMH))
	if log {
    fmt.Printf("%s: Start providing cid %v\n", time.Now().Format(time.RFC3339Nano), key.String())
	}


	// add self locally
	dht.providerStore.AddProvider(ctx, keyMH, peer.AddrInfo{ID: dht.self})
	if !brdcst {
		return nil
	}

	closerCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		timeout := deadline.Sub(now)

		if timeout < 0 {
			// timed out
			return context.DeadlineExceeded
		} else if timeout < 10*time.Second {
			// Reserve 10% for the final put.
			deadline = deadline.Add(-timeout / 10)
		} else {
			// Otherwise, reserve a second (we'll already be
			// connected so this should be fast).
			deadline = deadline.Add(-time.Second)
		}
		var cancel context.CancelFunc
		closerCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	var exceededDeadline bool
	if log {
    fmt.Printf("%s: Start getting closest peers to cid %v\n", time.Now().Format(time.RFC3339Nano), key.String())
	}
	peers, err := func(ctx context.Context, key string) ([]peer.ID, error) {
		if key == "" {
			return nil, fmt.Errorf("can't lookup empty key")
		}
		//TODO: I can break the interface! return []peer.ID
		lookupRes, err := dht.runLookupWithFollowup(ctx, key,
			func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type: routing.SendingQuery,
					ID:   p,
				})
				agentVersion := "n.a."
				if log {
					if agent, err := dht.peerstore.Get(p, "AgentVersion"); err == nil {
						agentVersion = agent.(string)
					}
          fmt.Printf("%s: Getting closest peers for cid %v from %v(%v)\n", time.Now().Format(time.RFC3339Nano), peer.ID(key).String(), p.String(), agentVersion)
				}

				peers, err := dht.protoMessenger.GetClosestPeers(ctx, p, peer.ID(key))
				if err != nil {
					logger.Debugf("error getting closer peers: %s", err)
					return nil, err
				}

				if log {
          msg := fmt.Sprintf("%s: Got %v closest peers to cid %v from %v(%v): ", time.Now().Format(time.RFC3339Nano), len(peers), peer.ID(key).String(), p.String(), agentVersion)
					for _, peer := range peers {
						msg = fmt.Sprintf("%v %v", msg, peer.ID.String())
					}
					fmt.Println(msg)
				}

				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type:      routing.PeerResponse,
					ID:        p,
					Responses: peers,
				})

				return peers, err
			},
			func() bool { return false },
		)

		if err != nil {
			return nil, err
		}

		if ctx.Err() == nil && lookupRes.completed {
			// refresh the cpl for this key as the query was successful
			dht.routingTable.ResetCplRefreshedAtForID(kb.ConvertKey(key), time.Now())
		}

		return lookupRes.peers, ctx.Err()
	}(closerCtx, string(keyMH))


	switch err {
	case context.DeadlineExceeded:
		// If the _inner_ deadline has been exceeded but the _outer_
		// context is still fine, provide the value to the closest peers
		// we managed to find, even if they're not the _actual_ closest peers.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		exceededDeadline = true
	case nil:
	default:
		return err
	}
	if log {
    fmt.Printf("%s: In total, got %v closest peers to cid %v to publish record: ", time.Now().Format(time.RFC3339Nano), len(peers), key.String())
		for _, peer := range peers {
			agentVersion := "n.a."
			if agent, err := dht.peerstore.Get(peer, "AgentVersion"); err == nil {
				agentVersion = agent.(string)
			}
			fmt.Printf("%v(%v) ", peer.String(), agentVersion)
		}
		fmt.Println()
	}

	wg := sync.WaitGroup{}
	for _, p := range peers {
		wg.Add(1)
		go func(p peer.ID) {
			defer wg.Done()
			logger.Debugf("putProvider(%s, %s)", internal.LoggableProviderRecordBytes(keyMH), p)
			agentVersion := "n.a."
			if agent, err := dht.peerstore.Get(p, "AgentVersion"); err == nil {
				agentVersion = agent.(string)
			}
			start := time.Now()
			if log {
				fmt.Printf("%s: Start putting provider record for cid %v to %v(%v)\n", time.Now().Format(time.RFC3339Nano), key.String(), p.String(), agentVersion)
			}
			err := dht.protoMessenger.PutProvider(ctx, p, keyMH, dht.host)
			if err != nil {
				if log {
					errStr := strings.ReplaceAll(err.Error(), "\n", " ")
          fmt.Printf("%s: Error putting provider record for cid %v to %v(%v) [%v] time taken: %v\n", time.Now().Format(time.RFC3339Nano), key.String(), p.String(), agentVersion, errStr, time.Since(start))
				}
				logger.Debug(err)
			} else {
				if log {
          fmt.Printf("%s: Succeed in putting provider record for cid %v to %v(%v) time taken: %v\n", time.Now().Format(time.RFC3339Nano), key.String(), p.String(), agentVersion, time.Since(start))
					// Now try to get the record from the peer
					pvds, _, err := dht.protoMessenger.GetProviders(ctx, p, keyMH)
					if err != nil {
            fmt.Printf("%s: Error getting provider record for cid %v from %v(%v) after a successful put %v\n", time.Now().Format(time.RFC3339Nano), key.String(), p.String(), agentVersion, err.Error())
					} else {
            msg := fmt.Sprintf("%s: Got %v provider records back from %v(%v) after a successful put: ", time.Now().Format(time.RFC3339Nano), len(pvds), p.String(), agentVersion)
						for _, pvd := range pvds {
							msg = fmt.Sprintf("%v %v", msg, pvd.ID.String())
						}
						fmt.Println(msg)
					}
				}
			}
		}(p)
	}
	wg.Wait()
	if log {
    fmt.Printf("%s: Finish providing cid %v\n", time.Now().Format(time.RFC3339Nano), key.String())
		activeTestingLock.Lock()
		delete(activeTesting, key.String())
		activeTestingLock.Unlock()
		os.WriteFile(path.Join(ipfsTestFolder, fmt.Sprintf("ok-provide-%v", key.String())), []byte{0}, os.ModePerm)
	}
	if exceededDeadline {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

// FindProviders searches until the context expires.
func (dht *IpfsDHT) FindProviders(ctx context.Context, c cid.Cid) ([]peer.AddrInfo, error) {
	if !dht.enableProviders {
		return nil, routing.ErrNotSupported
	} else if !c.Defined() {
		return nil, fmt.Errorf("invalid cid: undefined")
	}

	var providers []peer.AddrInfo
	for p := range dht.FindProvidersAsync(ctx, c, dht.bucketSize) {
		providers = append(providers, p)
	}
	return providers, nil
}

// FindProvidersAsync is the same thing as FindProviders, but returns a channel.
// Peers will be returned on the channel as soon as they are found, even before
// the search query completes. If count is zero then the query will run until it
// completes. Note: not reading from the returned channel may block the query
// from progressing.
func (dht *IpfsDHT) FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan peer.AddrInfo {
	if !dht.enableProviders || !key.Defined() {
		peerOut := make(chan peer.AddrInfo)
		close(peerOut)
		return peerOut
	}

	chSize := count
	if count == 0 {
		chSize = 1
	}
	peerOut := make(chan peer.AddrInfo, chSize)

	keyMH := key.Hash()

	logger.Debugw("finding providers", "cid", key, "mh", internal.LoggableProviderRecordBytes(keyMH))
	go dht.findProvidersAsyncRoutine(ctx, keyMH, count, peerOut)
	return peerOut
}

func (dht *IpfsDHT) findProvidersAsyncRoutine(ctx context.Context, key multihash.Multihash, count int, peerOut chan peer.AddrInfo) {
	defer close(peerOut)

	findAll := count == 0

	ps := make(map[peer.ID]struct{})
	psLock := &sync.Mutex{}
	psTryAdd := func(p peer.ID) bool {
		psLock.Lock()
		defer psLock.Unlock()
		_, ok := ps[p]
		if !ok && (len(ps) < count || findAll) {
			ps[p] = struct{}{}
			return true
		}
		return false
	}
	psSize := func() int {
		psLock.Lock()
		defer psLock.Unlock()
		return len(ps)
	}

	log := false
	ipfsTestFolder := os.Getenv("PERFORMANCE_TEST_DIR")
	if ipfsTestFolder == "" {
		ipfsTestFolder = "/ipfs-tests"
	}

	if _, err := os.Stat(path.Join(ipfsTestFolder, fmt.Sprintf("in-progress-lookup-%v", key.B58String()))); err == nil {
		os.Remove(path.Join(ipfsTestFolder, fmt.Sprintf("in-progress-lookup-%v", key.B58String())))
		log = true
		activeTestingLock.Lock()
		if activeTesting == nil {
			activeTesting = map[string]bool{}
		}
		_, ok := activeTesting[key.B58String()]
		if ok {
			// There is an active testing on.
			activeTestingLock.Unlock()
			return
		} else {
			activeTesting[key.B58String()] = true
		}
		activeTestingLock.Unlock()
	} else if _, err := os.Stat(path.Join(ipfsTestFolder, fmt.Sprintf("lookup-%v", key.B58String()))); err == nil {
		// Skip the second search.
		return
	}

	if !log {
		provs, err := dht.providerStore.GetProviders(ctx, key)
		if err != nil {
			return
		}
		for _, p := range provs {
			// NOTE: Assuming that this list of peers is unique
			if psTryAdd(p.ID) {
				select {
				case peerOut <- p:
				case <-ctx.Done():
					return
				}
			}

			// If we have enough peers locally, don't bother with remote RPC
			// TODO: is this a DOS vector?
			if !findAll && len(ps) >= count {
				return
			}
		}
	}

	if log {
    fmt.Printf("%s: Start searching providers for cid %v\n", time.Now().Format(time.RFC3339Nano), key.B58String())
	}

	lookupRes, err := dht.runLookupWithFollowup(ctx, string(key),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})
			agentVersion := "n.a."
			if log {
				if agent, err := dht.peerstore.Get(p, "AgentVersion"); err == nil {
					agentVersion = agent.(string)
				}
        fmt.Printf("%s: Getting providers for cid %v from %v(%v)\n", time.Now().Format(time.RFC3339Nano), key.B58String(), p.String(), agentVersion)
			}
			provs, closest, err := dht.protoMessenger.GetProviders(ctx, p, key)
			if err != nil {
				return nil, err
			}

			logger.Debugf("%d provider entries", len(provs))
			if log {
        fmt.Printf("%s: Found %v provider entries from from %v(%v)\n", time.Now().Format(time.RFC3339Nano), len(provs), p.String(), agentVersion)
			}

			// Add unique providers from request, up to 'count'
			for _, prov := range provs {
				dht.maybeAddAddrs(prov.ID, prov.Addrs, peerstore.TempAddrTTL)
				if log {
          fmt.Printf("%s: Found provider for cid %v from %v(%v): %v\n", time.Now().Format(time.RFC3339Nano), key.B58String(), p.String(), agentVersion, prov.ID.String())
				}

				logger.Debugf("got provider: %s", prov)
				if psTryAdd(prov.ID) {
					if log {
						agentVersion2 := "n.a."
						if agent, err := dht.peerstore.Get(prov.ID, "AgentVersion"); err == nil {
							agentVersion2 = agent.(string)
						}
            fmt.Printf("%s: Connected to provider %v(%v) for cid %v from %v(%v)\n", time.Now().Format(time.RFC3339Nano), prov.ID.String(), agentVersion2, key.B58String(), p.String(), agentVersion)
					}

					logger.Debugf("using provider: %s", prov)
					select {
					case peerOut <- *prov:
					case <-ctx.Done():
						logger.Debug("context timed out sending more providers")
						return nil, ctx.Err()
					}
				}
				if !findAll && psSize() >= count {
					logger.Debugf("got enough providers (%d/%d)", psSize(), count)
					return nil, nil
				}
			}

			// Give closer peers back to the query to be queried
			logger.Debugf("got closer peers: %d %s", len(closest), closest)
			if log {
        fmt.Printf("%s: Got %v closest peers to cid %v from %v(%v): ", time.Now().Format(time.RFC3339Nano), len(closest), key.B58String(), p.String(), agentVersion)
				for _, peer := range closest {
					fmt.Printf("%v ", peer.ID.String())
				}
				fmt.Println()
			}

			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: closest,
			})

			return closest, nil
		},
		func() bool {
			return !findAll && psSize() >= count
		},
	)

	if log {
		if ctx.Err() == nil {
      fmt.Printf("%s: Finish searching providers for cid %v\n", time.Now().Format(time.RFC3339Nano), key.B58String())
		} else {
      fmt.Printf("%s: Finish searching providers for cid %v with ctx error: %v\n", time.Now().Format(time.RFC3339Nano), key.B58String(), ctx.Err())
		}
		activeTestingLock.Lock()
		delete(activeTesting, key.B58String())
		activeTestingLock.Unlock()
	}

	if err == nil && ctx.Err() == nil {
		dht.refreshRTIfNoShortcut(kb.ConvertKey(string(key)), lookupRes)
	}
}

// FindPeer searches for a peer with given ID.
func (dht *IpfsDHT) FindPeer(ctx context.Context, id peer.ID) (_ peer.AddrInfo, err error) {
	if err := id.Validate(); err != nil {
		return peer.AddrInfo{}, err
	}

	logger.Debugw("finding peer", "peer", id)

	// Check if were already connected to them
	if pi := dht.FindLocal(id); pi.ID != "" {
		return pi, nil
	}

	lookupRes, err := dht.runLookupWithFollowup(ctx, string(id),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			peers, err := dht.protoMessenger.GetClosestPeers(ctx, p, id)
			if err != nil {
				logger.Debugf("error getting closer peers: %s", err)
				return nil, err
			}

			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, err
		},
		func() bool {
			return dht.host.Network().Connectedness(id) == network.Connected
		},
	)

	if err != nil {
		return peer.AddrInfo{}, err
	}

	dialedPeerDuringQuery := false
	for i, p := range lookupRes.peers {
		if p == id {
			// Note: we consider PeerUnreachable to be a valid state because the peer may not support the DHT protocol
			// and therefore the peer would fail the query. The fact that a peer that is returned can be a non-DHT
			// server peer and is not identified as such is a bug.
			dialedPeerDuringQuery = (lookupRes.state[i] == qpeerset.PeerQueried || lookupRes.state[i] == qpeerset.PeerUnreachable || lookupRes.state[i] == qpeerset.PeerWaiting)
			break
		}
	}

	// Return peer information if we tried to dial the peer during the query or we are (or recently were) connected
	// to the peer.
	connectedness := dht.host.Network().Connectedness(id)
	if dialedPeerDuringQuery || connectedness == network.Connected || connectedness == network.CanConnect {
		return dht.peerstore.PeerInfo(id), nil
	}

	return peer.AddrInfo{}, routing.ErrNotFound
}
