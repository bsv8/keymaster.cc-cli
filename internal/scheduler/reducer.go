package scheduler

import "math/big"

func ensureSeller(s *State, id string, online bool) *Seller {
	if s.Sellers[id] == nil {
		s.Sellers[id] = &Seller{ID: id, Online: online}
	}
	return s.Sellers[id]
}

// recordFailedAttempt accumulates already-transferred bytes and elapsed time.
// A zero-byte attempt stays at zero speed but is measured.
func recordFailedAttempt(seller *Seller, bytes, startedAt, at int64) {
	seller.BytesTotal += bytes
	elapsed := at - startedAt
	if elapsed < 1 {
		elapsed = 1
	}
	seller.TimeMsTotal += elapsed
	seller.Measured = true
	seller.LastAt = &at
}

func addContent(s *State, content []Content, reused []string) {
	for _, item := range content {
		identity(item.ID)
		if item.Kind != "seed" && item.Kind != "block" {
			fail("invalid content: %q", item.Kind)
		}
		integer(item.Bytes, 1)
		if previous, ok := s.Content.Get(item.ID); ok && (previous.Bytes != item.Bytes || previous.Kind != item.Kind) {
			fail("conflicting content identity: %s", item.ID)
		}
		s.Content.Set(item)
	}
	seedCount := 0
	for _, item := range s.Content.Values() {
		if item.Kind == "seed" {
			seedCount++
		}
	}
	if seedCount > 1 {
		fail("multiple Seed objects")
	}
	for _, id := range reused {
		if _, ok := s.Content.Get(id); !ok {
			fail("unknown reused content: %s", id)
		}
		for _, request := range s.Requests {
			if (request.State == "pending" || request.State == "unresolved") && containsString(request.ContentIDs, id) {
				fail("reusing unresolved content: %s", id)
			}
		}
		if _, ok := s.Completed[id]; !ok {
			s.Completed[id] = "reused"
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func boolValue(value *bool) bool { return value != nil && *value }

func intValue(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

func reduce(s *State, event Event) {
	switch event.Type {
	case "start", "resume":
		if s.Intent != "cancel" {
			s.Intent = "run"
		}
	case "pause":
		if s.Intent != "cancel" {
			s.Intent = "pause"
		}
	case "cancel":
		s.Intent = "cancel"
	case "tick":
		// Wake-up only.
	case "limits":
		if event.MaxBlockPrice != nil {
			sats(*event.MaxBlockPrice)
			s.Config.MaxBlockPrice = *event.MaxBlockPrice
		}
		if event.BaseSeats != nil {
			integer(*event.BaseSeats, 1)
			s.Config.BaseSeats = *event.BaseSeats
		}
		if event.FloatingSeats != nil {
			integer(*event.FloatingSeats, 1)
			s.Config.FloatingSeats = *event.FloatingSeats
		}
	case "slider":
		sats(event.Value)
		if event.Slider != "price" {
			fail("invalid slider")
		}
		value := event.Value
		s.Price = Slider{Mode: "manual", ManualValue: &value}
	case "automatic":
		if event.Slider != "price" {
			fail("invalid slider")
		}
		s.Price.Mode = "auto"
	case "manifest":
		addContent(s, event.Content, event.Reused)
		s.ManifestKnown = true
	case "quote":
		if event.Quote == nil {
			fail("invalid quote")
		}
		validateQuote(*event.Quote)
		quote := cloneQuote(*event.Quote)
		seller := ensureSeller(s, quote.SellerID, true)
		if seller.Quote == nil || quote.Version > seller.Quote.Version {
			seller.Quote = &quote
		}
	case "seller":
		identity(event.SellerID)
		ensureSeller(s, event.SellerID, false).Online = boolValue(event.Online)
	case "wallet":
		if event.Revision == nil {
			fail("invalid wallet revision")
		}
		integer(*event.Revision, 0)
		sats(event.Available)
		if *event.Revision > s.Wallet.Revision {
			s.Wallet = Wallet{
				Revision:          *event.Revision,
				Available:         event.Available,
				ReflectedCommands: append([]string{}, event.ReflectedCommands...),
			}
		}
	case "fees":
		if event.Fees == nil {
			fail("invalid fee contract")
		}
		validateFees(*event.Fees)
		pool := s.Pools[event.PoolID]
		if pool == nil {
			fail("unknown fee contract: %s", event.PoolID)
		}
		if event.Fees.Version > pool.Fees.Version {
			pool.Fees = *event.Fees
		}
	case "opened":
		pool := s.Pools[event.PoolID]
		if pool == nil || pool.OpenCommandID != event.CommandID {
			fail("uncorrelated opening: %s", event.PoolID)
		}
		if pool.OpeningObserved {
			break
		}
		if pool.Funding != "opening" && pool.Funding != "problem" {
			break
		}
		pool.OpeningObserved = true
		s.Spent = addSats(s.Spent, pool.Quote.Fees.Opening)
		if pool.Funding == "opening" {
			pool.Funding = "open"
		}
	case "open-failed":
		pool := s.Pools[event.PoolID]
		if pool == nil || pool.OpenCommandID != event.CommandID {
			fail("uncorrelated failure: %s", event.PoolID)
		}
		if pool.OpeningObserved || (pool.Funding != "opening" && pool.Funding != "problem") {
			break
		}
		if boolValue(event.Released) {
			pool.Funding = "failed"
		} else {
			pool.Funding = "problem"
		}
		pool.Scheduling = "retired"
	case "unknown":
		pool := s.Pools[event.PoolID]
		if pool == nil {
			fail("unknown pool result: %s", event.PoolID)
		}
		var request *Request
		for _, candidate := range s.Requests {
			if candidate.PoolID == pool.ID && candidate.CommandID == event.CommandID {
				request = candidate
				break
			}
		}
		if request != nil && (request.State == "settled" || request.State == "rejected") {
			break
		}
		if event.CommandID == pool.OpenCommandID && pool.OpeningObserved {
			break
		}
		correlated := event.CommandID == pool.OpenCommandID ||
			(pool.CloseCommandID != nil && event.CommandID == *pool.CloseCommandID) ||
			(request != nil && event.CommandID == request.CommandID)
		if !correlated {
			fail("uncorrelated unknown result: %s", event.CommandID)
		}
		if pool.Funding == "closed" || pool.Funding == "failed" || pool.Funding == "problem" {
			break
		}
		if request != nil {
			request.State = "unresolved"
		}
		pool.Funding = "problem"
		pool.Scheduling = "retired"
	case "progress":
		identity(event.RequestID)
		request := s.Requests[event.RequestID]
		if request == nil || request.State != "pending" {
			break
		}
		if event.Bytes == nil {
			fail("invalid transfer progress")
		}
		integer(*event.Bytes, 0)
		var size int64
		for _, id := range request.ContentIDs {
			item, _ := s.Content.Get(id)
			size += item.Bytes
		}
		if *event.Bytes > size {
			fail("impossible transfer progress")
		}
		if *event.Bytes > request.TransferredBytes {
			request.TransferredBytes = *event.Bytes
			request.LastProgressAt = s.Now
		}
	case "delivery":
		identity(event.RequestID)
		request := s.Requests[event.RequestID]
		if request == nil {
			fail("unknown delivery request: %s", event.RequestID)
		}
		if request.State == "settled" {
			break
		}
		pool := s.Pools[request.PoolID]
		if pool.Funding == "closed" || pool.Funding == "failed" {
			fail("delivery after pool settlement")
		}
		integer(event.StartedAt, 0)
		integer(event.EndedAt, event.StartedAt+1)
		if event.EndedAt > s.Now {
			fail("future delivery")
		}
		seen := make(map[string]bool, len(event.Accepted))
		for _, id := range event.Accepted {
			if seen[id] || !containsString(request.ContentIDs, id) {
				fail("invalid accepted content")
			}
			seen[id] = true
		}
		expected := big.NewInt(0)
		for _, id := range event.Accepted {
			expected.Add(expected, sats(request.Prices[id]))
		}
		if sats(event.Paid).Cmp(expected) != 0 {
			fail("payment differs from authorization")
		}
		committed := sats(pool.Committed)
		committed.Add(committed, expected)
		if committed.Cmp(sats(pool.Amount)) > 0 {
			fail("pool overspend")
		}
		pool.Committed = committed.String()
		for _, id := range event.Accepted {
			item, _ := s.Content.Get(id)
			if item.Kind == "block" {
				pool.PurchasedBlocks++
			}
		}
		s.Spent = addSats(s.Spent, expected.String())
		request.State = "settled"
		pool.RequestID = nil
		for _, id := range event.Accepted {
			s.Completed[id] = "purchased"
		}
		seller := s.Sellers[pool.SellerID]
		var acceptedBytes int64
		for _, id := range event.Accepted {
			item, _ := s.Content.Get(id)
			if item.Kind == "block" {
				acceptedBytes += item.Bytes
			}
		}
		if len(event.Accepted) == 0 {
			seller.Blacklisted = true
			seller.FailureCount++
			recordFailedAttempt(seller, 0, event.StartedAt, event.EndedAt)
		} else if acceptedBytes > 0 {
			seller.BytesTotal += acceptedBytes
			seller.TimeMsTotal += event.EndedAt - event.StartedAt
			seller.Measured = true
			seller.LastAt = &event.EndedAt
		}
		if pool.Funding == "open" && pool.Scheduling != "draining" && pool.Scheduling != "retired" {
			pool.Scheduling = "serving"
		}
	case "request-rejected":
		request := s.Requests[event.RequestID]
		if request == nil || (request.State != "pending" && request.State != "unresolved") {
			break
		}
		pool := s.Pools[request.PoolID]
		request.State = "rejected"
		pool.RequestID = nil
		if !boolValue(event.SafeToRetry) {
			seller := s.Sellers[pool.SellerID]
			seller.Blacklisted = true
			seller.FailureCount++
		}
		if pool.Scheduling != "retired" {
			pool.Scheduling = "draining"
			reason := "request-rejected"
			pool.DrainReason = &reason
		}
	case "closed":
		pool := s.Pools[event.PoolID]
		if pool == nil || pool.CloseCommandID == nil || *pool.CloseCommandID != event.CommandID || pool.RequestID != nil {
			fail("uncorrelated or unsafe close: %s", event.PoolID)
		}
		if pool.Funding == "closed" {
			break
		}
		total := new(big.Int).Set(sats(event.Returned))
		total.Add(total, sats(event.Spent))
		total.Add(total, sats(pool.Committed))
		if total.Cmp(sats(pool.Amount)) != 0 {
			fail("close money mismatch")
		}
		pool.Funding = "closed"
		pool.Scheduling = "retired"
		returned := event.Returned
		pool.Returned = &returned
		s.Spent = addSats(s.Spent, event.Spent)
	case "refund-available":
		pool := s.Pools[event.PoolID]
		if pool == nil || pool.Funding != "closed" {
			fail("refund before close observation")
		}
		pool.RefundAvailable = true
	default:
		fail("unknown scheduler event: %q", event.Type)
	}
}

func assertRequestPrices(s *State) {
	for _, request := range s.Requests {
		pool := s.Pools[request.PoolID]
		for _, id := range request.ContentIDs {
			item, _ := s.Content.Get(id)
			expected := price(pool.Quote, item)
			if sats(request.Prices[id]).Cmp(expected) != 0 {
				fail("corrupt request prices")
			}
		}
	}
}
