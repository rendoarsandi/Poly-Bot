const https = require('https');
const fs = require('fs');

const URL = 'https://polymarket.com/@distinct-baguette';

https.get(URL, (res) => {
    let data = '';
    res.on('data', chunk => data += chunk);
    res.on('end', () => {
        // We're looking for the Next.js hydration data script block
        // typically: <script id="__NEXT_DATA__" type="application/json">...</script>
        const match = data.match(/<script id="__NEXT_DATA__" type="application\/json">([\s\S]*?)<\/script>/);
        if (match && match[1]) {
            try {
                const jsonData = JSON.parse(match[1]);
                fs.writeFileSync('profile_data.json', JSON.stringify(jsonData, null, 2));
                console.log("Successfully extracted and saved profile JSON to profile_data.json");
                
                // Let's do a quick peek at what's in there
                const props = jsonData.props?.pageProps;
                if (props) {
                    console.log("PageProps keys:", Object.keys(props));
                    if (props.dehydratedState) {
                        console.log("DehydratedState queries length:", props.dehydratedState.queries?.length);
                    }
                }
            } catch (e) {
                console.error("Failed to parse JSON:", e);
            }
        } else {
            console.log("Could not find __NEXT_DATA__ script block.");
            // Write the raw html to see what we actually got
            fs.writeFileSync('profile_raw.html', data);
        }
    });
}).on('error', err => {
    console.error("Request failed:", err);
});
