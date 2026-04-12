const https = require('https');

const ADDRESS = '0xe0229e10a858860218b6132f4234602c47bd6603';
const URL = `https://data-api.polymarket.com/positions?user=${ADDRESS}`;

console.log(`Fetching detailed position data for ${ADDRESS}...\n`);

https.get(URL, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        try {
            const positions = JSON.parse(data);
            
            if (!Array.isArray(positions)) {
                console.error("Unexpected API response format.");
                return;
            }

            console.log(`=== Position Analysis for @distinct-baguette ===`);
            console.log(`Total Positions Found: ${positions.length}\n`);

            // Filter for positions with significant size to ignore dust
            const activePositions = positions.filter(p => p.size > 1);
            
            console.log(`Active/Significant Positions (${activePositions.length}):`);
            
            let totalInvested = 0;
            let totalCurrentValue = 0;

            activePositions.sort((a, b) => b.size - a.size).forEach(p => {
                totalInvested += p.initialValue;
                totalCurrentValue += p.currentValue;
                
                console.log(`\nMarket: ${p.title} (${p.outcome})`);
                console.log(`  Size:        ${p.size.toFixed(2)} shares`);
                console.log(`  Avg Price:   $${p.avgPrice.toFixed(4)}`);
                console.log(`  Cur Price:   $${p.curPrice.toFixed(4)}`);
                console.log(`  Total Bought:${p.totalBought.toFixed(2)}`);
                console.log(`  Invested:    $${p.initialValue.toFixed(2)}`);
                console.log(`  PnL:         $${p.cashPnl.toFixed(2)} (${p.percentPnl.toFixed(2)}%)`);
                
                // Grid/Limit Analysis Heuristics
                const spread = Math.abs(p.curPrice - p.avgPrice);
                let strategy = "Unknown";
                
                if (p.totalBought > p.size * 2) {
                    strategy = "High-Frequency Churn (Bought and sold heavily on this outcome)";
                } else if (p.avgPrice < 0.05 || p.avgPrice > 0.95) {
                    strategy = "Deep Out-of-the-Money (OTM) Limit Sniper";
                } else if (spread < 0.05) {
                    strategy = "Tight Mid-Market Peg (AMM Maker)";
                } else if (p.totalBought > 0) {
                    strategy = "Directional/Grid Accumulation";
                }
                
                console.log(`  Strategy:    ${strategy}`);
            });

            console.log(`\n=== Portfolio Summary ===`);
            console.log(`Total Invested in Active Positions: $${totalInvested.toFixed(2)}`);
            console.log(`Total Current Value:                $${totalCurrentValue.toFixed(2)}`);
            console.log(`Overall Active PnL:                 $${(totalCurrentValue - totalInvested).toFixed(2)}`);

        } catch (e) {
            console.error("Failed to parse API response:", e.message);
        }
    });
}).on("error", (err) => {
    console.log("Error: " + err.message);
});
