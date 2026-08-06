const { CommClient } = require('caspian-sdk');
require('dotenv').config({ path: '../.env' });

const client = new CommClient({
    apiKey: process.env.CASPIAN_API_KEY,
    baseUrl: process.env.CASPIAN_BASE_URL
});

async function run() {
    try {
        console.log("Connecting email channel...");
        const conn = await client.connectEmail({
            username: "shuttlecourt"
        });
        console.log("Email channel connected successfully!");
        console.log("Connection details:", JSON.stringify(conn, null, 2));
    } catch (e) {
        console.error("Error connecting email:", e.message);
    }
}
run();
