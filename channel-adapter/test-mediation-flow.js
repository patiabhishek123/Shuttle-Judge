const axios = require('axios');

const BACKEND_URL = 'http://localhost:8080';

async function sendMsg(convId, sender, text) {
    console.log(`\n[${sender}] -> "${text}"`);
    const resp = await axios.post(`${BACKEND_URL}/message`, {
        conversation_id: convId,
        channel: 'email',
        text: text,
        sender_ref: sender
    });
    console.log(`[Docket] -> "${resp.data.reply_text.replace(/\n/g, ' ')}"`);
    return resp.data.reply_text;
}

async function run() {
    try {
        console.log("=== STARTING DYNAMIC SHUTTLE COURT MEDIATION FLOW TEST ===");

        const testId = Date.now();
        const convA = `conv_party_a_${testId}`;
        const convB = `conv_party_b_${testId}`;

        // --- PARTY A INTAKE ---
        let reply = await sendMsg(
            convA, 
            'party_a@example.com', 
            'I want to open a case about our electricity bill from March. I paid $340 but the other party refuses to pay their half.'
        );

        // Extract join code
        const joinCodeMatch = reply.match(/join code with the other party: \*\*([A-Z0-9]+)\*\*/i);
        if (!joinCodeMatch) {
            throw new Error("Failed to extract join code from welcome message");
        }
        const joinCode = joinCodeMatch[1];
        console.log(`>>> Extracted Join Code: ${joinCode}`);

        // Party A interaction loop
        let attempts = 0;
        while (attempts < 5) {
            attempts++;
            if (reply.includes("is that right?") || reply.includes("is this correct?")) {
                reply = await sendMsg(convA, 'party_a@example.com', 'Yes, that is correct.');
                break;
            } else if (reply.includes("waiting for the other party to join")) {
                break;
            } else {
                reply = await sendMsg(convA, 'party_a@example.com', 'I paid $340 on March 15 for electricity. I want them to pay half ($170).');
            }
        }

        // --- PARTY B JOINS ---
        reply = await sendMsg(
            convB,
            'party_b@example.com',
            joinCode
        );

        // Party B interaction loop
        attempts = 0;
        let contradictionResolved = false;
        while (attempts < 10) {
            attempts++;
            if (reply.includes("Do you accept this proposal?") || reply.includes("resolution proposal:")) {
                console.log(">>> Proposal received by Party B!");
                break;
            } else if (reply.includes("is that right?") || reply.includes("is this correct?")) {
                reply = await sendMsg(convB, 'party_b@example.com', 'Yes, that is correct.');
            } else if (reply.includes("double check") || reply.includes("receipt") || reply.includes("exactly") || reply.includes("amount") || reply.includes("Who paid")) {
                if (!contradictionResolved) {
                    reply = await sendMsg(convB, 'party_b@example.com', 'Ah, sorry, looking at the bill statement again, it was indeed $340. I was looking at the February one.');
                    contradictionResolved = true;
                } else {
                    reply = await sendMsg(convB, 'party_b@example.com', 'It was definitely $340.');
                }
            } else {
                reply = await sendMsg(convB, 'party_b@example.com', 'I paid $150 for March electricity. The bill was actually $300 total.');
            }
        }

        // --- CONSENT PHASE ---
        // A votes YES
        reply = await sendMsg(
            convA,
            'party_a@example.com',
            'YES'
        );
		if (!reply.toLowerCase().includes('resolved')) {
			throw new Error(`Expected final RESOLVED confirmation, received: ${reply}`);
		}

        // B votes YES
        reply = await sendMsg(
            convB,
            'party_b@example.com',
            'YES'
        );

        console.log("\n=== DYNAMIC FLOW TEST COMPLETED SUCCESSFULLY ===");
    } catch (e) {
        console.error("Test failed:", e.message);
        if (e.response && e.response.data) {
            console.error("Backend error details:", e.response.data);
        }
		process.exitCode = 1;
    }
}

run();
