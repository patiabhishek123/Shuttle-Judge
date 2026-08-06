const { CommClient } = require('caspian-sdk');
require('dotenv').config({ path: '../.env' });

const client = new CommClient({
    apiKey: process.env.CASPIAN_API_KEY,
    baseUrl: process.env.CASPIAN_BASE_URL
});

async function run() {
    try {
        console.log("Fetching active connections...");
        const res = await client.request("GET", "/v1/connections");
        console.log("Connections:", JSON.stringify(res, null, 2));
    } catch (e) {
        console.error("Error:", e.message);
    }
}
run();
