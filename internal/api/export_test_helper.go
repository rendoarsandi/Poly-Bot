package api
func MarketFromGammaEventTokenIDForTesting(event *GammaEvent, tokenID string) (Market, string, bool) {
	return marketFromGammaEventTokenID(event, tokenID)
}
