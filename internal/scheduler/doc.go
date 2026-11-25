// Package scheduler is the extension point that decides which deployment
// serves a request for the model "auto". It is a leaf package: it imports
// only the standard library, so a strategy never depends on the gateway,
// the store or Oakestra.
//
// # Adding a strategy
//
// Create one new file in this package (or any package that the server
// imports for side effects), implement [Strategy] and register it from an
// init function. Nothing in the gateway, the settings schema or the admin
// API changes; the strategy is then selectable by name through the
// "scheduler" setting or OOA_SCHEDULER.
//
// A complexity classifier, for example, asks a small deployed decision model
// how hard the prompt is and keeps only the candidates labelled for that
// tier:
//
//	func init() {
//		Register("complexity", func(cfg json.RawMessage, d Deps) (Strategy, error) {
//			var c struct{ Classifier string `json:"classifier"` }
//			if err := json.Unmarshal(cfg, &c); err != nil {
//				return nil, err
//			}
//			return &complexity{classifier: c.Classifier, deps: d}, nil
//		})
//	}
//
//	func (s *complexity) Rank(ctx context.Context, r Request, c []Candidate) ([]Candidate, error) {
//		tier, err := s.deps.Complete(ctx, s.classifier, []Message{
//			{Role: "system", Content: "Answer with one word: easy or hard."},
//			{Role: "user", Content: r.Prompt},
//		})
//		if err != nil {
//			return nil, err // the gateway falls back to least-inflight
//		}
//		var matching []Candidate
//		for _, cand := range c {
//			if cand.Labels["tier"] == strings.TrimSpace(tier) {
//				matching = append(matching, cand)
//			}
//		}
//		return RankLeastInflight(matching), nil
//	}
//
// Rank may drop candidates. The gateway walks the returned order when an
// engine cannot be reached, gives Rank a short time budget, and falls back
// to [LeastInflight] when Rank returns an error or nothing.
package scheduler
