const https = require('https');
const fs = require('fs');

const USER = '0xd189664C5308903476f9F079820431E4fD7D06F4';
const LIMIT = 1000;
const URL = `https://data-api.polymarket.com/trades?user=${USER}&limit=${LIMIT}`;

https.get(URL, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        try {
            const parsed = JSON.parse(data);
            fs.writeFileSync('new_wallet_trades.json', JSON.stringify(parsed, null, 2));
            console.log(`Successfully fetched ${parsed.length} trades for the new wallet.`);
        } catch (e) {
            console.error("Failed to parse JSON:", e.message);
        }
    });
}).on('error', err => {
    console.error("Request failed:", err.message);
});
