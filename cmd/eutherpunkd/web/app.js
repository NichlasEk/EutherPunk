const statusEl = document.querySelector("#status");
const messagesEl = document.querySelector("#messages");
const form = document.querySelector("#chatForm");
const promptEl = document.querySelector("#prompt");
const micButton = document.querySelector("#micButton");
const voiceToggle = document.querySelector("#voiceToggle");
const serverVoiceToggle = document.querySelector("#serverVoiceToggle");

let ttsEnabled = false;
let serverVoiceEnabled = false;
let recognition = null;
let activeAudio = null;

async function loadStatus() {
  try {
    const response = await fetch("/api/eutherpunk/status");
    const status = await response.json();
    statusEl.textContent = `${status.service} · ${status.model} · ${status.users} user`;
  } catch (error) {
    statusEl.textContent = `Offline: ${error.message}`;
  }
}

function addMessage(role, text = "") {
  const node = document.createElement("article");
  node.className = `message ${role}`;
  node.textContent = text;
  messagesEl.appendChild(node);
  messagesEl.scrollTop = messagesEl.scrollHeight;
  return node;
}

async function speak(text) {
  if (!ttsEnabled || !text.trim()) {
    return;
  }
  if (serverVoiceEnabled) {
    await speakWithServerVoice(text);
    return;
  }
  if (!("speechSynthesis" in window)) {
    return;
  }
  const utterance = new SpeechSynthesisUtterance(text);
  utterance.lang = "sv-SE";
  window.speechSynthesis.cancel();
  window.speechSynthesis.speak(utterance);
}

async function speakWithServerVoice(text) {
  const response = await fetch("/api/eutherpunk/tts", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text }),
  });
  if (!response.ok) {
    const message = await response.text();
    throw new Error(message || response.statusText);
  }
  const audioBlob = await response.blob();
  const url = URL.createObjectURL(audioBlob);
  if (activeAudio) {
    activeAudio.pause();
    URL.revokeObjectURL(activeAudio.src);
  }
  activeAudio = new Audio(url);
  activeAudio.onended = () => URL.revokeObjectURL(url);
  await activeAudio.play();
}

async function sendPrompt(prompt) {
  addMessage("user", prompt);
  const assistantNode = addMessage("assistant", "");
  let fullText = "";

  const response = await fetch("/api/eutherpunk/chat/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ message: prompt }),
  });

  if (!response.ok || !response.body) {
    const text = await response.text();
    throw new Error(text || response.statusText);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n");
    buffer = lines.pop() || "";
    for (const line of lines) {
      if (!line.trim()) {
        continue;
      }
      const chunk = JSON.parse(line);
      if (chunk.error) {
        throw new Error(chunk.error);
      }
      if (chunk.delta) {
        fullText += chunk.delta;
        assistantNode.textContent = fullText;
        messagesEl.scrollTop = messagesEl.scrollHeight;
      }
    }
  }

  await speak(fullText);
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const prompt = promptEl.value.trim();
  if (!prompt) {
    return;
  }
  promptEl.value = "";
  form.querySelector("button[type='submit']").disabled = true;
  try {
    await sendPrompt(prompt);
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  } finally {
    form.querySelector("button[type='submit']").disabled = false;
    promptEl.focus();
  }
});

voiceToggle.addEventListener("click", () => {
  ttsEnabled = !ttsEnabled;
  voiceToggle.textContent = ttsEnabled ? "TTS på" : "TTS av";
});

serverVoiceToggle.addEventListener("click", () => {
  serverVoiceEnabled = !serverVoiceEnabled;
  serverVoiceToggle.textContent = serverVoiceEnabled ? "Serverröst på" : "Serverröst av";
  if (serverVoiceEnabled && !ttsEnabled) {
    ttsEnabled = true;
    voiceToggle.textContent = "TTS på";
  }
});

function setupSpeechRecognition() {
  const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (!SpeechRecognition) {
    micButton.disabled = true;
    micButton.title = "Voice input stöds inte i den här browsern";
    return;
  }
  recognition = new SpeechRecognition();
  recognition.lang = "sv-SE";
  recognition.interimResults = false;
  recognition.onresult = (event) => {
    const transcript = event.results?.[0]?.[0]?.transcript || "";
    promptEl.value = transcript;
    promptEl.focus();
  };
  recognition.onend = () => {
    micButton.disabled = false;
  };
}

micButton.addEventListener("click", () => {
  if (!recognition) {
    return;
  }
  micButton.disabled = true;
  recognition.start();
});

setupSpeechRecognition();
loadStatus();
