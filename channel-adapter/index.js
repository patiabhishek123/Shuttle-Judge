const { CommClient } = require('caspian-sdk');
const axios = require('axios');
require('dotenv').config({ path: '../.env' });

const client = new CommClient();
const GO_BACKEND_URL = process.env.GO_BACKEND_URL || 'http://localhost:8080';

async function main() {
    try {
        console.log("Starting Caspian channel adapter...");
        console.log("Forwarding to Go backend at:", GO_BACKEND_URL);

        // Connect Email channel (requires no credentials)
        const emailUsername = process.env.EMAIL_USERNAME || 'shuttlecourt';
        console.log(`Connecting Email channel with username: ${emailUsername}...`);
        const emailConn = await client.connectEmail({ username: emailUsername });
        console.log("Email connected successfully. Address:", emailConn.address);

        // Optionally connect Telegram if bot token is provided
        if (process.env.TELEGRAM_BOT_TOKEN) {
            console.log("Connecting Telegram channel using TELEGRAM_BOT_TOKEN...");
            try {
                const tgConn = await client.connectTelegram({ botToken: process.env.TELEGRAM_BOT_TOKEN });
                console.log("Telegram bot connected successfully. Address:", tgConn.address);
            } catch (err) {
                console.error("Failed to connect Telegram:", err.message);
            }
        } else {
            console.log("No TELEGRAM_BOT_TOKEN found in env. Skipping Telegram connection.");
        }

        // Optionally connect Slack if Slack credentials are provided
        if (process.env.SLACK_CLIENT_ID && process.env.SLACK_CLIENT_SECRET && process.env.SLACK_SIGNING_SECRET) {
            console.log("Connecting Slack channel using branded credentials...");
            try {
                const slackConn = await client.connectSlack({
                    slackClientId: process.env.SLACK_CLIENT_ID,
                    slackClientSecret: process.env.SLACK_CLIENT_SECRET,
                    slackSigningSecret: process.env.SLACK_SIGNING_SECRET
                });
                console.log("Slack OAuth connection initialized. Authorize URL:", slackConn.authorize_url);
            } catch (err) {
                console.error("Failed to initialize Slack branded app:", err.message);
            }
        } else if (process.env.SLACK_QUICK_NAME) {
            console.log(`Connecting Slack channel using shared app name: ${process.env.SLACK_QUICK_NAME}...`);
            try {
                const slackConn = await client.installSlack({ displayName: process.env.SLACK_QUICK_NAME });
                console.log("Slack shared app connection initialized. Authorize URL:", slackConn.authorize_url);
            } catch (err) {
                console.error("Failed to initialize Slack shared app:", err.message);
            }
        } else {
            console.log("No Slack config found in env. Skipping Slack connection.");
        }

        // Register message handler
        client.onMessage(async (message) => {
            console.log(`\n--- Received Message ---`);
            console.log(`ID: ${message.id}`);
            console.log(`Channel: ${message.channel}`);
            console.log(`Conv ID: ${message.conversationId}`);
            console.log(`Text bytes: ${Buffer.byteLength(message.text || '', 'utf8')}`);
            
            // Format a simple sender reference string for the backend
            let senderRef = 'unknown';
            if (message.sender) {
                if (typeof message.sender === 'object') {
                    senderRef = message.sender.address || message.sender.username || message.sender.id || JSON.stringify(message.sender);
                } else {
                    senderRef = String(message.sender);
                }
            }

            try {
                // Forward message to Go mediation engine
                const response = await axios.post(`${GO_BACKEND_URL}/message`, {
                    conversation_id: message.conversationId,
                    channel: message.channel,
                    text: message.text || '',
                    sender_ref: senderRef
                }, {
                    timeout: 30000,
                    headers: process.env.ENGINE_TOKEN ? { Authorization: `Bearer ${process.env.ENGINE_TOKEN}` } : {}
                });

                const replyText = response.data.reply_text || '';
                console.log(`Replying with: "${replyText}"`);
                
                // Send reply back via Caspian
                await message.reply(replyText);
            } catch (err) {
                console.error("Error communicating with Go backend or replying:", err.message);
                if (err.response && err.response.data) {
                    console.error("Backend error detail:", JSON.stringify(err.response.data));
                }
                await message.reply("Sorry, I encountered an internal error processing your message.");
            }
        });

        // Start local HTTP server for proactive sending from Go backend
        const http = require('http');
        const proactiveServer = http.createServer((req, res) => {
            if (req.method === 'POST' && req.url === '/send') {
                if (process.env.ADAPTER_TOKEN && req.headers.authorization !== `Bearer ${process.env.ADAPTER_TOKEN}`) {
                    res.writeHead(401, { 'Content-Type': 'application/json' });
                    res.end(JSON.stringify({ error: 'Unauthorized' }));
                    return;
                }
                let body = '';
                let tooLarge = false;
                req.on('data', chunk => {
                    if (!tooLarge) body += chunk;
                    if (Buffer.byteLength(body, 'utf8') > 65536) {
                        tooLarge = true;
                    }
                });
                req.on('end', async () => {
                    if (tooLarge) {
                        res.writeHead(413, { 'Content-Type': 'application/json' });
                        res.end(JSON.stringify({ error: 'Payload too large' }));
                        return;
                    }
                    try {
                        const payload = JSON.parse(body);
                        const { conversation_id, text } = payload;
                        if (!conversation_id || !text) {
                            res.writeHead(400, { 'Content-Type': 'application/json' });
                            res.end(JSON.stringify({ error: 'Missing conversation_id or text' }));
                            return;
                        }
                        console.log(`[Proactive] Sending to conversation ending ${String(conversation_id).slice(-4)} (${Buffer.byteLength(text, 'utf8')} bytes)`);
                        if (conversation_id.startsWith('conv_')) {
                            console.log(`[Proactive] Mock conversation detected. Simulating send success.`);
                        } else {
                            await client.sendMessage(conversation_id, text);
                        }
                        res.writeHead(200, { 'Content-Type': 'application/json' });
                        res.end(JSON.stringify({ success: true }));
                    } catch (err) {
                        console.error("[Proactive] Send failed:", err.message);
                        res.writeHead(500, { 'Content-Type': 'application/json' });
                        res.end(JSON.stringify({ error: err.message }));
                    }
                });
            } else {
                res.writeHead(404);
                res.end();
            }
        });

        const ADAPTER_PORT = process.env.ADAPTER_PORT || 8081;
        proactiveServer.listen(ADAPTER_PORT, () => {
            console.log(`Adapter proactive send server listening on port ${ADAPTER_PORT}`);
        });

        // Start listening
        console.log("Listening for Caspian events...");
        await client.listen();

    } catch (err) {
        console.error("Fatal error in channel adapter:", err);
        process.exit(1);
    }
}

main();
