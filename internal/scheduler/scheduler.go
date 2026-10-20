package scheduler

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// CreateInput is the explicit task creation input.
type CreateInput struct {
	TaskID        string    `json:"taskId"`
	SeedHash      string    `json:"seedHash"`
	Config        Config    `json:"config"`
	Content       []Content `json:"content"`
	ManifestKnown bool      `json:"manifestKnown"`
	Reused        []string  `json:"reused"`
}

// CreateTask validates the input and returns a fresh task state.
func CreateTask(input CreateInput) (state State, err error) {
	defer recoverTo(&err)
	identity(input.TaskID)
	identity(input.SeedHash)
	validateConfig(input.Config)
	state = State{
		Version:       1,
		TaskID:        input.TaskID,
		SeedHash:      input.SeedHash,
		Revision:      0,
		Sequence:      0,
		Now:           0,
		Config:        input.Config,
		Intent:        "ready",
		Price:         Slider{Mode: "auto"},
		Seats:         []string{},
		ManifestKnown: input.ManifestKnown,
		Content:       newContentStore(),
		Completed:     map[string]string{},
		Sellers:       map[string]*Seller{},
		Pools:         map[string]*Pool{},
		Requests:      map[string]*Request{},
		Wallet:        Wallet{Revision: -1, Available: "0", ReflectedCommands: []string{}},
		Spent:         "0",
		SeenEvents:    map[string]bool{},
	}
	addContent(&state, input.Content, input.Reused)
	return state, nil
}

// RestoreTask validates and clones a persisted snapshot.
func RestoreTask(snapshot State) (state State, err error) {
	defer recoverTo(&err)
	clone, err := cloneState(snapshot)
	if err != nil {
		return State{}, err
	}
	s := &clone
	if s.Version != 1 || s.TaskID == "" || s.SeedHash == "" {
		fail("unsupported task snapshot")
	}
	validateConfig(s.Config)
	integer(s.Now, 0)
	integer(s.Revision, 0)
	integer(s.Sequence, 0)
	identity(s.TaskID)
	identity(s.SeedHash)
	if s.Intent != "ready" && s.Intent != "run" && s.Intent != "pause" && s.Intent != "cancel" {
		fail("invalid snapshot intent")
	}
	sats(s.Spent)
	sats(s.Wallet.Available)
	if s.Price.Mode != "auto" && s.Price.Mode != "manual" {
		fail("invalid snapshot slider")
	}
	if s.Price.ManualValue != nil {
		sats(*s.Price.ManualValue)
	}
	if s.Price.Mode == "manual" && s.Price.ManualValue == nil {
		fail("missing manual target")
	}
	if s.Seats == nil {
		fail("invalid snapshot seats")
	}
	for _, id := range s.Seats {
		identity(id)
		if s.Sellers[id] == nil {
			fail("unknown seat seller: %s", id)
		}
	}
	for _, item := range s.Content.Values() {
		if item.ID == "" {
			fail("content key mismatch")
		}
	}
	addContent(s, s.Content.Values(), nil)
	for id := range s.Completed {
		if _, ok := s.Content.Get(id); !ok {
			fail("unknown completed content: %s", id)
		}
	}
	for id, seller := range s.Sellers {
		identity(id)
		if seller.ID != id {
			fail("seller key mismatch")
		}
		if seller.Quote != nil {
			validateQuote(*seller.Quote)
			if seller.Quote.SellerID != id {
				fail("quote seller mismatch")
			}
		}
		integer(seller.FailureCount, 0)
		integer(seller.BytesTotal, 0)
		integer(seller.TimeMsTotal, 0)
		if seller.Measured && seller.TimeMsTotal <= 0 {
			fail("measured seller without recorded time")
		}
		if seller.LastAt != nil {
			integer(*seller.LastAt, 0)
		}
	}
	for _, pool := range s.Pools {
		if pool.TaskID != s.TaskID {
			fail("cross-task pool snapshot")
		}
		validateQuote(pool.Quote)
		validateFees(pool.Fees)
		if s.Sellers[pool.SellerID] == nil || pool.Quote.SellerID != pool.SellerID {
			fail("missing pool seller")
		}
		if pool.Funding != "opening" && pool.Funding != "open" && pool.Funding != "closing" &&
			pool.Funding != "closed" && pool.Funding != "failed" && pool.Funding != "problem" {
			fail("invalid funding state")
		}
		integer(pool.BlockCapacity, 0)
		integer(pool.PurchasedBlocks, 0)
		if pool.PurchasedBlocks > pool.BlockCapacity {
			fail("pool block capacity exceeded")
		}
		sats(pool.Amount)
		sats(pool.Committed)
		if sats(pool.Committed).Cmp(sats(pool.Amount)) > 0 {
			fail("corrupt pool commitment")
		}
		if pool.RequestID != nil && s.Requests[*pool.RequestID] == nil {
			fail("missing unresolved request")
		}
	}
	claims := map[string]bool{}
	for _, request := range s.Requests {
		if s.Pools[request.PoolID] == nil {
			fail("orphan request")
		}
		for _, id := range request.ContentIDs {
			if _, ok := s.Content.Get(id); !ok {
				fail("orphan request content")
			}
		}
		seen := map[string]bool{}
		for _, id := range request.ContentIDs {
			if seen[id] {
				fail("duplicate request content")
			}
			seen[id] = true
		}
		if request.State == "pending" || request.State == "unresolved" {
			pool := s.Pools[request.PoolID]
			if pool.RequestID == nil || *pool.RequestID != request.ID {
				fail("request ownership mismatch")
			}
			for _, id := range request.ContentIDs {
				if claims[id] {
					fail("duplicate outstanding content")
				}
				if _, completed := s.Completed[id]; completed {
					fail("duplicate outstanding content")
				}
				claims[id] = true
			}
		}
	}
	assertRequestPrices(s)
	return *s, nil
}

// Step applies one event and returns the new state, commands and view.
func Step(previous State, event Event) (result Result, err error) {
	defer recoverTo(&err)
	identity(event.EventID)
	if event.TaskID != previous.TaskID || event.EventID == "" {
		fail("wrong task event")
	}
	integer(event.ReceivedAt, 0)
	integer(event.OccurredAt, 0)
	if event.OccurredAt > event.ReceivedAt {
		fail("event received before occurrence")
	}
	if previous.SeenEvents[event.EventID] {
		return Result{State: previous, Commands: []Command{}, View: view(&previous), NextWakeAt: nextWakeAt(&previous)}, nil
	}
	if event.ReceivedAt < previous.Now {
		fail("receive clock moved backwards")
	}
	clone, err := cloneState(previous)
	if err != nil {
		return Result{}, err
	}
	s := &clone
	s.Now = event.ReceivedAt
	s.Revision++
	s.SeenEvents[event.EventID] = true
	reduce(s, event)
	commands := plan(s)
	return Result{State: *s, Commands: commands, View: view(s), NextWakeAt: nextWakeAt(s)}, nil
}

// NextWakeAt returns the earliest host wake-up time (or nil).
func NextWakeAt(s State) *int64 { return nextWakeAt(&s) }

// ProjectView returns the host/UI projection of a state (same projection Step emits).
func ProjectView(s State) View { return view(&s) }

func nextWakeAt(s *State) *int64 {
	times := make([]int64, 0)
	for _, seller := range s.Sellers {
		if seller.Quote != nil && seller.Quote.ExpiresAt > s.Now {
			times = append(times, seller.Quote.ExpiresAt)
		}
	}
	for _, pool := range sortedPools(s) {
		if pool.RequestID == nil {
			continue
		}
		request := s.Requests[*pool.RequestID]
		if request != nil && request.State == "pending" {
			times = append(times, request.LastProgressAt+s.Config.RequestTimeoutMs)
		}
	}
	future := make([]int64, 0, len(times))
	for _, value := range times {
		if value > s.Now {
			future = append(future, value)
		}
	}
	if len(future) == 0 {
		return nil
	}
	minimum := future[0]
	for _, value := range future[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return &minimum
}

func plan(s *State) []Command {
	commands := make([]Command, 0)

	// Stalled requests end first: the seller records a zero/short attempt and
	// the pool drains.
	for _, request := range sortedRequests(s) {
		if request.State != "pending" || s.Now < request.LastProgressAt+s.Config.RequestTimeoutMs {
			continue
		}
		pool := s.Pools[request.PoolID]
		request.State = "rejected"
		pool.RequestID = nil
		recordFailedAttempt(s.Sellers[pool.SellerID], request.TransferredBytes, request.StartedAt, s.Now)
		drain(s, pool, &commands, "request-timeout")
	}

	_, quotes, effective := recommendations(s)
	complete := s.ManifestKnown && len(pendingContent(s)) == 0
	enabled := s.Intent == "run" && !complete
	awaitingManifest := !s.ManifestKnown && len(pendingContent(s)) == 0
	livePools := make([]*Pool, 0)
	for _, pool := range sortedPools(s) {
		if isLive(pool) {
			livePools = append(livePools, pool)
		}
	}
	qualified := []Quote{}
	if enabled {
		qualified = quotes
	}
	qualifiedIDs := map[string]bool{}
	for _, quote := range qualified {
		qualifiedIDs[quote.SellerID] = true
	}

	desired := newOrderedSet()
	for _, id := range s.Seats {
		if qualifiedIDs[id] {
			desired.add(id)
		}
	}

	// Seat shrink: keep the best seats within the cap and the unmeasured
	// floating limit; extra seats exit via drain only.
	trimmed := map[string]bool{}
	{
		order := desired.list()
		sort.SliceStable(order, func(i, j int) bool { return bestSeatFirst(s)(order[i], order[j]) < 0 })
		keep := 0
		unmeasured := int64(0)
		for _, id := range order {
			archived := hasArchive(s, id)
			if int64(keep) >= seatCap(s) || (!archived && unmeasured >= unmeasuredCap(s)) {
				trimmed[id] = true
				continue
			}
			keep++
			if !archived {
				unmeasured++
			}
		}
		for id := range trimmed {
			desired.delete(id)
		}
	}

	claimed := map[string]bool{}
	for _, request := range sortedRequests(s) {
		if request.State == "pending" || request.State == "unresolved" {
			for _, id := range request.ContentIDs {
				claimed[id] = true
			}
		}
	}
	drainedSellers := make([]string, 0)
	drainedSeen := map[string]bool{}
	for _, pool := range livePools {
		if pool.Funding != "open" || pool.RequestID != nil {
			continue
		}
		if pool.Scheduling == "draining" {
			continue
		}
		if !enabled || awaitingManifest {
			continue
		}
		if !desired.has(pool.SellerID) {
			continue
		}
		if sats(pool.Quote.BlockPrice).Cmp(effective) > 0 {
			continue
		}
		items := make([]Content, 0)
		for _, item := range availableContent(s, pool.Quote) {
			if !claimed[item.ID] {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			seedFirst := items[0].Kind == "seed"
			remaining := pool.BlockCapacity - pool.PurchasedBlocks
			if remaining < 0 {
				remaining = 0
			}
			if seedFirst {
				items = items[:1]
			} else if int64(len(items)) > remaining {
				items = items[:remaining]
			}
		}
		for len(items) > 0 {
			sum := big.NewInt(0)
			for _, item := range items {
				sum.Add(sum, price(pool.Quote, item))
			}
			sum.Add(sum, reserve(pool.Fees, int64(len(items))))
			availableAmount := new(big.Int).Sub(sats(pool.Amount), sats(pool.Committed))
			if sum.Cmp(availableAmount) <= 0 {
				break
			}
			items = items[:len(items)-1]
		}
		if len(items) == 0 {
			hasUnclaimed := false
			for _, item := range availableContent(s, pool.Quote) {
				if !claimed[item.ID] {
					hasUnclaimed = true
					break
				}
			}
			if hasUnclaimed {
				drain(s, pool, &commands, "pool-capacity")
				if !drainedSeen[pool.SellerID] {
					drainedSeen[pool.SellerID] = true
					drainedSellers = append(drainedSellers, pool.SellerID)
				}
			}
			continue
		}
		requestID := nextID(s, "request")
		prices := make(map[string]Sats, len(items))
		maxPayment := big.NewInt(0)
		contentIDs := make([]string, 0, len(items))
		for _, item := range items {
			value := price(pool.Quote, item)
			prices[item.ID] = value.String()
			maxPayment.Add(maxPayment, value)
			contentIDs = append(contentIDs, item.ID)
		}
		reason := "download"
		if items[0].Kind == "seed" {
			reason = "seed-first"
		}
		command := base(s, reason)
		command.Type = "download"
		command.PoolID = pool.ID
		command.RequestID = requestID
		command.ContentIDs = contentIDs
		command.Prices = prices
		command.MaxPayment = maxPayment.String()
		command.PriceLimit = effective.String()
		feeVersion := pool.Fees.Version
		command.FeeVersion = &feeVersion
		s.Requests[requestID] = &Request{
			ID:             requestID,
			CommandID:      command.CommandID,
			PoolID:         pool.ID,
			ContentIDs:     contentIDs,
			Prices:         prices,
			StartedAt:      s.Now,
			LastProgressAt: s.Now,
			State:          "pending",
		}
		pool.RequestID = &requestID
		pool.Scheduling = "serving"
		for _, item := range items {
			claimed[item.ID] = true
		}
		commands = append(commands, command)
	}

	if len(drainedSellers) > 0 && enabled {
		reviewSeats(s, drainedSellers, desired, qualified)
	}

	for _, pool := range livePools {
		if !enabled {
			reason := s.Intent
			if complete {
				reason = "content-complete"
			}
			drain(s, pool, &commands, reason)
			continue
		}
		if awaitingManifest {
			continue
		}
		if !desired.has(pool.SellerID) {
			reason := "strategy-change"
			if sats(pool.Quote.BlockPrice).Cmp(effective) > 0 {
				reason = "price-limit"
			}
			drain(s, pool, &commands, reason)
		}
	}

	for _, pool := range sortedPools(s) {
		if pool.Funding != "open" || pool.RequestID != nil || pool.Scheduling != "draining" {
			continue
		}
		reason := "drain-complete"
		if pool.DrainReason != nil {
			reason = *pool.DrainReason
		}
		command := base(s, reason)
		command.Type = "close"
		command.PoolID = pool.ID
		command.Committed = pool.Committed
		pool.Funding = "closing"
		closeID := command.CommandID
		pool.CloseCommandID = &closeID
		commands = append(commands, command)
	}

	if enabled && !awaitingManifest {
		cap := seatCap(s)
		if int64(desired.len()) < cap {
			measuredIdle := make([]Quote, 0)
			for _, quote := range qualified {
				if desired.has(quote.SellerID) || trimmed[quote.SellerID] || !hasArchive(s, quote.SellerID) {
					continue
				}
				measuredIdle = append(measuredIdle, quote)
			}
			sort.SliceStable(measuredIdle, func(i, j int) bool {
				left := sellerSpeed(s, measuredIdle[i].SellerID)
				right := sellerSpeed(s, measuredIdle[j].SellerID)
				if left != nil && right != nil && *left != *right {
					if *right > *left {
						return true
					}
					return false
				}
				return compareStrings(measuredIdle[i].SellerID, measuredIdle[j].SellerID) < 0
			})
			for _, quote := range measuredIdle {
				if int64(desired.len()) >= cap {
					break
				}
				desired.add(quote.SellerID)
			}
			unmeasured := int64(0)
			for _, id := range desired.list() {
				if !hasArchive(s, id) {
					unmeasured++
				}
			}
			unmeasuredIdle := make([]string, 0)
			for _, quote := range qualified {
				if desired.has(quote.SellerID) || trimmed[quote.SellerID] || hasArchive(s, quote.SellerID) {
					continue
				}
				unmeasuredIdle = append(unmeasuredIdle, quote.SellerID)
			}
			for int64(desired.len()) < cap && unmeasured < unmeasuredCap(s) && len(unmeasuredIdle) > 0 {
				chosen := pickRandom(s, unmeasuredIdle)
				for index, id := range unmeasuredIdle {
					if id == chosen {
						unmeasuredIdle = append(unmeasuredIdle[:index], unmeasuredIdle[index+1:]...)
						break
					}
				}
				desired.add(chosen)
				unmeasured++
			}
		}
		for _, id := range desired.list() {
			activePool := false
			for _, pool := range livePools {
				if pool.SellerID == id && pool.Scheduling != "draining" && pool.Scheduling != "retired" {
					activePool = true
					break
				}
			}
			if activePool {
				continue
			}
			var quote *Quote
			for index := range qualified {
				if qualified[index].SellerID == id {
					quote = &qualified[index]
					break
				}
			}
			if quote == nil {
				continue
			}
			seller := s.Sellers[id]
			newQuote := seller.Quote
			if newQuote == nil || newQuote.ExpiresAt <= s.Now || newQuote.ID != quote.ID || newQuote.Version != quote.Version {
				continue
			}
			items := make([]Content, 0)
			for _, item := range pendingContent(s) {
				if claimed[item.ID] {
					continue
				}
				if quote.ContentIDs != nil && !containsString(quote.ContentIDs, item.ID) {
					continue
				}
				items = append(items, item)
			}
			if len(items) == 0 {
				continue
			}
			blocks := make([]Content, 0)
			seeds := make([]Content, 0)
			for _, item := range items {
				if item.Kind == "block" {
					blocks = append(blocks, item)
				} else {
					seeds = append(seeds, item)
				}
			}
			count := int64(len(blocks))
			if quote.MaxBlocks < count {
				count = quote.MaxBlocks
			}
			if s.Config.PoolBlocks < count {
				count = s.Config.PoolBlocks
			}
			var budget, amount *big.Int
			for count >= 0 {
				budget = big.NewInt(0)
				for _, item := range seeds {
					budget.Add(budget, price(*quote, item))
				}
				for index := int64(0); index < count; index++ {
					budget.Add(budget, price(*quote, blocks[index]))
				}
				reserveCount := count
				if reserveCount < 1 {
					reserveCount = 1
				}
				amount = new(big.Int).Add(budget, reserve(quote.Fees, reserveCount))
				total := new(big.Int).Add(amount, sats(quote.Fees.Opening))
				if total.Cmp(walletFree(s)) <= 0 {
					break
				}
				count--
			}
			if count < 0 {
				continue
			}
			if count == 0 && len(seeds) == 0 {
				continue
			}
			poolID := nextID(s, "pool")
			command := base(s, "selected-seller")
			command.Type = "open"
			command.PoolID = poolID
			command.SellerID = quote.SellerID
			command.SessionID = quote.SessionID
			quoteCopy := cloneQuote(*quote)
			command.Quote = &quoteCopy
			command.Amount = amount.String()
			command.ContentBudget = budget.String()
			command.Reserve = new(big.Int).Sub(amount, budget).String()
			command.OpeningFee = quote.Fees.Opening
			walletRevision := s.Wallet.Revision
			command.WalletRevision = &walletRevision
			blockCapacity := count
			command.BlockCapacity = &blockCapacity
			s.Pools[poolID] = &Pool{
				ID:                     poolID,
				TaskID:                 s.TaskID,
				SellerID:               quote.SellerID,
				SessionID:              quote.SessionID,
				Quote:                  quoteCopy,
				Fees:                   quote.Fees,
				Amount:                 command.Amount,
				Committed:              "0",
				BlockCapacity:          count,
				PurchasedBlocks:        0,
				Funding:                "opening",
				Scheduling:             "idle",
				OpenCommandID:          command.CommandID,
				CloseCommandID:         nil,
				RequestID:              nil,
				DrainReason:            nil,
				Returned:               nil,
				RefundAvailable:        false,
				OpeningObserved:        false,
				WalletRevisionAtIntent: s.Wallet.Revision,
			}
			commands = append(commands, command)
		}
	}
	s.Seats = desired.list()
	return commands
}

func validateConfig(config Config) {
	sats(config.MaxBlockPrice)
	integer(config.BaseSeats, 1)
	integer(config.FloatingSeats, 1)
	integer(config.PoolBlocks, 1)
	integer(config.RequestTimeoutMs, 1)
}

func isLive(pool *Pool) bool {
	return pool.Funding != "closed" && pool.Funding != "failed" && pool.Funding != "problem"
}

func nextID(s *State, kind string) string {
	s.Sequence++
	return fmt.Sprintf("%s/%s/%d", s.TaskID, kind, s.Sequence)
}

func base(s *State, reason string) Command {
	return Command{CommandID: nextID(s, "command"), TaskID: s.TaskID, Revision: s.Revision, Reason: reason}
}

func drain(s *State, pool *Pool, commands *[]Command, reason string) {
	if pool.Scheduling == "draining" || pool.Scheduling == "retired" {
		return
	}
	pool.Scheduling = "draining"
	pool.DrainReason = &reason
	command := base(s, reason)
	command.Type = "drain"
	command.PoolID = pool.ID
	*commands = append(*commands, command)
}

func walletFree(s *State) *big.Int {
	free := sats(s.Wallet.Available)
	for _, pool := range s.Pools {
		if pool.Funding == "failed" {
			continue
		}
		if containsString(s.Wallet.ReflectedCommands, pool.OpenCommandID) {
			continue
		}
		unreflected := new(big.Int).Add(sats(pool.Amount), sats(pool.Quote.Fees.Opening))
		free.Sub(free, unreflected)
	}
	return free
}

func hasInFlight(s *State, sellerID string) bool {
	for _, pool := range sortedPools(s) {
		if pool.SellerID != sellerID || pool.RequestID == nil {
			continue
		}
		request := s.Requests[*pool.RequestID]
		if request != nil && (request.State == "pending" || request.State == "unresolved") {
			return true
		}
	}
	return false
}

func bestSeatFirst(s *State) func(a, b string) int {
	return func(a, b string) int {
		if hasInFlight(s, b) != hasInFlight(s, a) {
			if hasInFlight(s, b) {
				return 1
			}
			return -1
		}
		if hasArchive(s, b) != hasArchive(s, a) {
			if hasArchive(s, b) {
				return 1
			}
			return -1
		}
		speedA := 0.0
		speedB := 0.0
		if value := sellerSpeed(s, a); value != nil {
			speedA = *value
		}
		if value := sellerSpeed(s, b); value != nil {
			speedB = *value
		}
		if speedA != speedB {
			if speedA > speedB {
				return -1
			}
			return 1
		}
		return compareStrings(a, b)
	}
}

func reviewSeats(s *State, drained []string, desired *orderedSet, qualified []Quote) {
	for _, drainSeller := range drained {
		if !desired.has(drainSeller) || !hasArchive(s, drainSeller) {
			continue
		}
		if hasInFlight(s, drainSeller) {
			continue
		}
		ranked := make([]string, 0)
		for _, id := range desired.list() {
			if hasArchive(s, id) {
				ranked = append(ranked, id)
			}
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			left := sellerSpeed(s, ranked[i])
			right := sellerSpeed(s, ranked[j])
			if left != nil && right != nil && *left != *right {
				if *right > *left {
					return true
				}
				return false
			}
			return compareStrings(ranked[i], ranked[j]) < 0
		})
		rank := int64(-1)
		for index, id := range ranked {
			if id == drainSeller {
				rank = int64(index + 1)
				break
			}
		}
		if rank <= s.Config.BaseSeats {
			continue
		}
		idle := make([]Quote, 0)
		for _, quote := range qualified {
			if !desired.has(quote.SellerID) {
				idle = append(idle, quote)
			}
		}
		measuredIdle := make([]Quote, 0)
		for _, quote := range idle {
			if hasArchive(s, quote.SellerID) {
				measuredIdle = append(measuredIdle, quote)
			}
		}
		sort.SliceStable(measuredIdle, func(i, j int) bool {
			left := sellerSpeed(s, measuredIdle[i].SellerID)
			right := sellerSpeed(s, measuredIdle[j].SellerID)
			if left != nil && right != nil && *left != *right {
				if *right > *left {
					return true
				}
				return false
			}
			return compareStrings(measuredIdle[i].SellerID, measuredIdle[j].SellerID) < 0
		})
		if len(measuredIdle) > 0 {
			candidate := *sellerSpeed(s, measuredIdle[0].SellerID)
			current := *sellerSpeed(s, drainSeller)
			if candidate > current {
				desired.delete(drainSeller)
				desired.add(measuredIdle[0].SellerID)
				continue
			}
		}
		unmeasuredInSeat := int64(0)
		for _, id := range desired.list() {
			if !hasArchive(s, id) {
				unmeasuredInSeat++
			}
		}
		unmeasuredIdle := make([]string, 0)
		for _, quote := range idle {
			if !hasArchive(s, quote.SellerID) {
				unmeasuredIdle = append(unmeasuredIdle, quote.SellerID)
			}
		}
		if len(unmeasuredIdle) > 0 && unmeasuredInSeat < s.Config.FloatingSeats {
			desired.delete(drainSeller)
			desired.add(pickRandom(s, unmeasuredIdle))
		}
	}
}

func pickRandom(s *State, ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	seed := fmt.Sprintf("%s|%d|%s", s.TaskID, s.Sequence, strings.Join(sorted, ","))
	hash := uint32(2166136261)
	for index := 0; index < len(seed); index++ {
		hash ^= uint32(seed[index])
		hash *= 16777619
	}
	return sorted[int(hash%uint32(len(sorted)))]
}

func cloneState(state State) (State, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return State{}, err
	}
	var clone State
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return State{}, err
	}
	return clone, nil
}

func recoverTo(err *error) {
	if recovered := recover(); recovered != nil {
		if kernel, ok := recovered.(kernelPanic); ok {
			*err = kernel.err
			return
		}
		panic(recovered)
	}
}

func sortedPools(s *State) []*Pool {
	ids := make([]string, 0, len(s.Pools))
	for id := range s.Pools {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := idSequence(ids[i]), idSequence(ids[j])
		if left != right {
			return left < right
		}
		return ids[i] < ids[j]
	})
	pools := make([]*Pool, 0, len(ids))
	for _, id := range ids {
		pools = append(pools, s.Pools[id])
	}
	return pools
}

func sortedRequests(s *State) []*Request {
	ids := make([]string, 0, len(s.Requests))
	for id := range s.Requests {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := idSequence(ids[i]), idSequence(ids[j])
		if left != right {
			return left < right
		}
		return ids[i] < ids[j]
	})
	requests := make([]*Request, 0, len(ids))
	for _, id := range ids {
		requests = append(requests, s.Requests[id])
	}
	return requests
}

func idSequence(id string) int64 {
	index := strings.LastIndex(id, "/")
	if index < 0 {
		return 0
	}
	value, err := strconv.ParseInt(id[index+1:], 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func compareStrings(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

type orderedSet struct {
	items []string
	set   map[string]bool
}

func newOrderedSet() *orderedSet {
	return &orderedSet{set: map[string]bool{}}
}

func (o *orderedSet) add(id string) {
	if o.set[id] {
		return
	}
	o.set[id] = true
	o.items = append(o.items, id)
}

func (o *orderedSet) delete(id string) {
	if !o.set[id] {
		return
	}
	delete(o.set, id)
	for index, value := range o.items {
		if value == id {
			o.items = append(o.items[:index], o.items[index+1:]...)
			return
		}
	}
}

func (o *orderedSet) has(id string) bool { return o.set[id] }

func (o *orderedSet) len() int { return len(o.items) }

func (o *orderedSet) list() []string {
	result := make([]string, len(o.items))
	copy(result, o.items)
	return result
}
