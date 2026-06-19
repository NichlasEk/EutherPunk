const statusEl = document.querySelector("#status");
const chatTitleEl = document.querySelector("#chatTitle");
const messagesEl = document.querySelector("#messages");
const conversationListEl = document.querySelector("#conversationList");
const newChatButton = document.querySelector("#newChatButton");
const form = document.querySelector("#chatForm");
const promptEl = document.querySelector("#prompt");
const imagePreviewEl = document.querySelector("#imagePreview");
const micButton = document.querySelector("#micButton");
const voiceToggle = document.querySelector("#voiceToggle");
const serverVoiceToggle = document.querySelector("#serverVoiceToggle");
const settingsButton = document.querySelector("#settingsButton");
const settingsDialog = document.querySelector("#settingsDialog");
const settingsForm = document.querySelector("#settingsForm");
const settingsCloseButton = document.querySelector("#settingsCloseButton");
const chatModelInput = document.querySelector("#chatModelInput");
const visionModelInput = document.querySelector("#visionModelInput");
const imageModelSelect = document.querySelector("#imageModelSelect");
const imageLoraSelect = document.querySelector("#imageLoraSelect");
const voiceBackendInput = document.querySelector("#voiceBackendInput");
const settingsSaveButton = document.querySelector("#settingsSaveButton");

const maxHistoryMessages = 30;
const maxVisionImageSide = 256;
const visionImageQuality = 0.72;
let ttsEnabled = false;
let serverVoiceEnabled = false;
let recognition = null;
let activeAudio = null;
let pendingImages = [];
let conversations = [];
let activeConversationId = "";
let activeConversationTitle = "Ny chat";
let activeConversationCreatedAt = "";
let conversationMessages = [];
let userSettings = {
  chat_model: "qwen3-coder:30b",
  vision_model: "moondream:latest",
  image_model: "z-image-turbo",
  image_lora: "none",
  voice_backend: "grapheneos-matcha-en",
  tts_enabled: false,
  server_voice_enabled: false,
};
let imageModels = [
  { id: "z-image-turbo", label: "Z-Image Turbo" },
  { id: "sensenova-u1-8b-fast", label: "SenseNova U1 8B snabb" },
  { id: "sensenova-u1-8b", label: "SenseNova U1 8B" },
];
let imageLoras = ["none"];

function newConversationId() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === "function") {
    return globalThis.crypto.randomUUID();
  }
  return `chat-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

async function loadStatus() {
  try {
    const response = await fetch("/api/eutherpunk/status");
    const status = await response.json();
    statusEl.textContent = `${status.service} · ${status.model} · ${status.users} user`;
  } catch (error) {
    statusEl.textContent = `Offline: ${error.message}`;
  }
}

async function loadConversationList() {
  const response = await fetch("/api/eutherpunk/conversations");
  if (!response.ok) {
    throw new Error(await response.text() || response.statusText);
  }
  const payload = await response.json();
  conversations = payload.conversations || [];
  renderConversationList();
}

async function loadSettings() {
  const response = await fetch("/api/eutherpunk/settings");
  if (!response.ok) {
    throw new Error(await response.text() || response.statusText);
  }
  const payload = await response.json();
  userSettings = { ...userSettings, ...(payload.settings || {}) };
  imageModels = payload.image_models || imageModels;
  imageLoras = userSettings.loras || imageLoras;
  ttsEnabled = Boolean(userSettings.tts_enabled);
  serverVoiceEnabled = Boolean(userSettings.server_voice_enabled);
  renderVoiceToggles();
  renderSettingsForm();
}

function renderSettingsForm() {
  chatModelInput.value = userSettings.chat_model || "";
  visionModelInput.value = userSettings.vision_model || "";
  voiceBackendInput.value = userSettings.voice_backend || "";
  renderOptions(imageModelSelect, imageModels.map((model) => [model.id, model.label]), userSettings.image_model);
  const selectedImageModel = imageModelSelect.value || userSettings.image_model;
  const selectedLora = selectedImageModel === "sensenova-u1-8b" ? userSettings.image_lora : "none";
  renderOptions(imageLoraSelect, imageLoras.map((lora) => [lora, lora === "none" ? "Ingen" : lora]), selectedLora);
  imageLoraSelect.disabled = selectedImageModel !== "sensenova-u1-8b";
}

function renderOptions(select, options, selectedValue) {
  select.replaceChildren();
  for (const [value, label] of options) {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    if (value === selectedValue) {
      option.selected = true;
    }
    select.appendChild(option);
  }
}

async function saveSettings() {
  const imageModel = imageModelSelect.value;
  const payload = {
    chat_model: chatModelInput.value.trim(),
    vision_model: visionModelInput.value.trim(),
    image_model: imageModel,
    image_lora: imageModel === "sensenova-u1-8b" ? imageLoraSelect.value : "none",
    voice_backend: voiceBackendInput.value.trim(),
    tts_enabled: ttsEnabled,
    server_voice_enabled: serverVoiceEnabled,
  };
  await saveSettingsPayload(payload);
}

async function saveRuntimeSettings() {
  await saveSettingsPayload({
    chat_model: userSettings.chat_model,
    vision_model: userSettings.vision_model,
    image_model: userSettings.image_model,
    image_lora: userSettings.image_model === "sensenova-u1-8b" ? userSettings.image_lora : "none",
    voice_backend: userSettings.voice_backend,
    tts_enabled: ttsEnabled,
    server_voice_enabled: serverVoiceEnabled,
  });
}

async function saveSettingsPayload(payload) {
  const response = await fetch("/api/eutherpunk/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  const body = await response.json();
  if (!response.ok) {
    throw new Error(body.error || response.statusText);
  }
  userSettings = { ...userSettings, ...(body.settings || {}) };
  imageLoras = userSettings.loras || imageLoras;
  ttsEnabled = Boolean(userSettings.tts_enabled);
  serverVoiceEnabled = Boolean(userSettings.server_voice_enabled);
  renderVoiceToggles();
  renderSettingsForm();
}

function renderVoiceToggles() {
  voiceToggle.textContent = ttsEnabled ? "TTS pa" : "TTS av";
  serverVoiceToggle.textContent = serverVoiceEnabled ? "Serverrost pa" : "Serverrost av";
}

async function openConversation(id) {
  const response = await fetch(`/api/eutherpunk/conversations/${encodeURIComponent(id)}`);
  if (!response.ok) {
    throw new Error(await response.text() || response.statusText);
  }
  const conversation = await response.json();
  activeConversationId = conversation.id;
  activeConversationTitle = conversation.title || "Ny chat";
  activeConversationCreatedAt = conversation.created_at || "";
  conversationMessages = normalizeStoredMessages(conversation.messages || []);
  renderConversation();
  renderConversationList();
}

function startNewConversation() {
  activeConversationId = "";
  activeConversationTitle = "Ny chat";
  activeConversationCreatedAt = "";
  conversationMessages = [];
  renderConversation();
  renderConversationList();
  promptEl.focus();
}

async function saveActiveConversation() {
  if (conversationMessages.length === 0) {
    return;
  }
  if (!activeConversationId) {
    activeConversationId = newConversationId();
  }
  const payload = {
    id: activeConversationId,
    title: activeConversationTitle,
    created_at: activeConversationCreatedAt,
    messages: conversationMessages,
  };
  const response = await fetch(`/api/eutherpunk/conversations/${encodeURIComponent(activeConversationId)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    throw new Error(await response.text() || response.statusText);
  }
  const saved = await response.json();
  activeConversationId = saved.id;
  activeConversationTitle = saved.title || activeConversationTitle;
  activeConversationCreatedAt = saved.created_at || activeConversationCreatedAt;
  upsertConversationSummary(saved);
  renderConversationList();
  renderTitle();
}

async function deleteStoredConversation(id) {
  const response = await fetch(`/api/eutherpunk/conversations/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
  if (!response.ok) {
    throw new Error(await response.text() || response.statusText);
  }
  conversations = conversations.filter((conversation) => conversation.id !== id);
  if (id === activeConversationId) {
    startNewConversation();
    return;
  }
  renderConversationList();
}

async function confirmAndDeleteConversation(id) {
  const conversation = conversations.find((item) => item.id === id);
  const title = conversation?.title || "chatten";
  if (!globalThis.confirm(`Ta bort "${title}"?`)) {
    return;
  }
  await deleteStoredConversation(id);
}

function upsertConversationSummary(conversation) {
  const summary = {
    id: conversation.id,
    title: conversation.title || "Ny chat",
    created_at: conversation.created_at,
    updated_at: conversation.updated_at,
    count: (conversation.messages || []).length,
  };
  const index = conversations.findIndex((item) => item.id === summary.id);
  if (index >= 0) {
    conversations[index] = summary;
  } else {
    conversations.unshift(summary);
  }
  conversations.sort((a, b) => new Date(b.updated_at || 0) - new Date(a.updated_at || 0));
}

function renderConversationList() {
  conversationListEl.replaceChildren();
  if (conversations.length === 0) {
    const empty = document.createElement("div");
    empty.className = "conversationItem is-empty";
    empty.innerHTML = "<strong>Ingen historik än</strong><small>Skriv första frågan</small>";
    conversationListEl.appendChild(empty);
    return;
  }
  for (const conversation of conversations) {
    const item = document.createElement("div");
    item.className = `conversationItem${conversation.id === activeConversationId ? " is-active" : ""}`;
    item.dataset.conversationId = conversation.id;
    item.tabIndex = 0;
    item.role = "button";
    const title = document.createElement("strong");
    title.textContent = conversation.title || "Ny chat";
    const meta = document.createElement("small");
    meta.textContent = conversationDate(conversation.updated_at);
    const deleteButton = document.createElement("button");
    deleteButton.type = "button";
    deleteButton.className = "conversationDelete";
    deleteButton.dataset.deleteConversationId = conversation.id;
    deleteButton.title = "Ta bort chat";
    deleteButton.setAttribute("aria-label", `Ta bort ${conversation.title || "chat"}`);
    deleteButton.textContent = "×";
    item.append(title, meta, deleteButton);
    conversationListEl.appendChild(item);
  }
}

function renderConversation() {
  messagesEl.replaceChildren();
  renderTitle();
  for (const message of conversationMessages) {
    addMessage(message.role, message.content, message.images || []);
  }
}

function renderTitle() {
  chatTitleEl.textContent = activeConversationTitle === "Ny chat" ? "EutherPunk" : activeConversationTitle;
}

function addMessage(role, text = "", images = []) {
  const node = document.createElement("article");
  node.className = `message ${role}`;
  if (text) {
    const textNode = document.createElement("div");
    textNode.textContent = text;
    node.appendChild(textNode);
  }
  if (images.length > 0) {
    const imageWrap = document.createElement("div");
    imageWrap.className = "messageImages";
    for (const image of images) {
      const img = document.createElement("img");
      img.src = image.url || image.dataURL;
      img.alt = image.alt || "Bild";
      imageWrap.appendChild(img);
    }
    node.appendChild(imageWrap);
  }
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
    body: JSON.stringify({ text, model_backend: userSettings.voice_backend }),
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

async function sendPrompt(prompt, images = []) {
  const userMessage = {
    role: "user",
    content: prompt || "Beskriv bilden.",
    images,
  };
  addMessage("user", userMessage.content, images);
  conversationMessages.push(userMessage);
  trimHistory();
  await saveActiveConversation();

  const assistantNode = addMessage("assistant", "");
  let fullText = "";

  try {
    const response = await fetch("/api/eutherpunk/chat/stream", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: userSettings.chat_model,
        messages: modelMessages(conversationMessages),
      }),
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
  } catch (error) {
    conversationMessages = conversationMessages.filter((message) => message !== userMessage);
    await saveActiveConversation().catch(() => {});
    throw error;
  }

  conversationMessages.push({ role: "assistant", content: fullText, images: [] });
  trimHistory();
  await saveActiveConversation();
  await speak(fullText);
}

async function generateImage(prompt, displayText = "") {
  const userMessage = { role: "user", content: displayText || `/bild ${prompt}`, images: [] };
  addMessage("user", userMessage.content);
  const assistantNode = addMessage("assistant", "Genererar bild...");
  conversationMessages.push(userMessage);
  trimHistory();
  await saveActiveConversation();

  try {
    const response = await fetch("/api/eutherpunk/images/generate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        prompt,
        image_model: userSettings.image_model,
        lora: userSettings.image_lora,
        context: modelMessages(conversationMessages.slice(-12)),
      }),
    });
    let payload = await readJSONResponse(response);
    if (!response.ok) {
      throw new Error(payload.error || response.statusText);
    }
    if (payload.job_id) {
      payload = await waitForImageJob(payload.job_id, assistantNode);
    }
    const generatedImage = { url: payload.url, alt: prompt };
    assistantNode.replaceChildren();
    const textNode = document.createElement("div");
    textNode.textContent = "Bild klar.";
    const imageWrap = document.createElement("div");
    imageWrap.className = "messageImages";
    const img = document.createElement("img");
    img.src = payload.url;
    img.alt = prompt;
    imageWrap.appendChild(img);
    assistantNode.append(textNode, imageWrap);
    conversationMessages.push({
      role: "assistant",
      content: `Bild sparad: ${payload.url}`,
      images: [generatedImage],
    });
    trimHistory();
    await saveActiveConversation();
  } catch (error) {
    conversationMessages = conversationMessages.filter((message) => message !== userMessage);
    await saveActiveConversation().catch(() => {});
    throw error;
  }
}

async function waitForImageJob(jobId, assistantNode) {
  const startedAt = Date.now();
  let transientFailures = 0;
  while (Date.now() - startedAt < 15 * 60 * 1000) {
    await sleep(2000);
    let response;
    try {
      response = await fetch(`/api/eutherpunk/images/jobs/${encodeURIComponent(jobId)}`);
      transientFailures = 0;
    } catch (error) {
      transientFailures += 1;
      if (transientFailures < 5) {
        continue;
      }
      throw error;
    }
    let job;
    try {
      job = await readJSONResponse(response);
    } catch (error) {
      transientFailures += 1;
      if (transientFailures < 10) {
        continue;
      }
      throw error;
    }
    transientFailures = 0;
    if (!response.ok) {
      throw new Error(job.error || response.statusText);
    }
    if (job.status === "done" && job.image) {
      return job.image;
    }
    if (job.status === "error") {
      throw new Error(job.error || "Bildjobbet misslyckades");
    }
    const elapsed = Math.round((Date.now() - startedAt) / 1000);
    assistantNode.textContent = job.status === "running" ? `Genererar bild... ${elapsed}s` : `Bildjobb i ko... ${elapsed}s`;
  }
  throw new Error("Bildjobbet tog for lang tid");
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function readJSONResponse(response) {
  const text = await response.text();
  if (!text.trim()) {
    throw new Error(response.ok ? "Tomt svar från servern" : response.statusText);
  }
  try {
    return JSON.parse(text);
  } catch (error) {
    const preview = text.replace(/\s+/g, " ").trim().slice(0, 180);
    throw new Error(`Servern svarade inte med JSON: ${preview || error.message}`);
  }
}

function trimHistory() {
  if (conversationMessages.length > maxHistoryMessages) {
    conversationMessages = conversationMessages.slice(-maxHistoryMessages);
  }
}

function modelMessages(messages) {
  return messages.map((message, index) => {
    const isLatest = index === messages.length - 1;
    const images = isLatest ? (message.images || []).map((image) => image.ollamaImage).filter(Boolean) : [];
    return {
      role: message.role,
      content: message.content,
      images,
    };
  });
}

function normalizeStoredMessages(messages) {
  return messages.map((message) => ({
    role: message.role,
    content: message.content || "",
    images: (message.images || []).map((image) => ({
      dataURL: image.dataURL || "",
      url: image.url || "",
      alt: image.alt || "Bild",
      ollamaImage: image.ollamaImage || image.ollama_image || "",
    })),
  }));
}

function conversationDate(value) {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return new Intl.DateTimeFormat("sv-SE", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

function renderImagePreview() {
  imagePreviewEl.replaceChildren();
  pendingImages.forEach((image, index) => {
    const item = document.createElement("div");
    item.className = "previewItem";
    const img = document.createElement("img");
    img.src = image.dataURL;
    img.alt = image.name || "Bild";
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "x";
    button.title = "Ta bort bild";
    button.addEventListener("click", () => {
      pendingImages.splice(index, 1);
      renderImagePreview();
      promptEl.focus();
    });
    item.append(img, button);
    imagePreviewEl.appendChild(item);
  });
}

async function addImageFile(file) {
  if (!file.type.startsWith("image/")) {
    return;
  }
  const dataURL = await compressImageFile(file, maxVisionImageSide, visionImageQuality);
  const [, base64 = ""] = dataURL.split(",", 2);
  if (!base64) {
    return;
  }
  pendingImages.push({
    name: file.name,
    dataURL,
    alt: file.name || "Bild",
    ollamaImage: base64,
  });
  renderImagePreview();
}

async function compressImageFile(file, maxSide, quality) {
  const bitmap = await loadBitmap(file);
  const scale = Math.min(1, maxSide / Math.max(bitmap.width, bitmap.height));
  const width = Math.max(1, Math.round(bitmap.width * scale));
  const height = Math.max(1, Math.round(bitmap.height * scale));
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext("2d");
  context.drawImage(bitmap, 0, 0, width, height);
  if ("close" in bitmap) {
    bitmap.close();
  }
  return canvas.toDataURL("image/jpeg", quality);
}

async function loadBitmap(file) {
  if ("createImageBitmap" in window) {
    return createImageBitmap(file);
  }
  const dataURL = await readFileAsDataURL(file);
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error("Kunde inte lasa bild"));
    img.src = dataURL;
  });
}

function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ""));
    reader.onerror = () => reject(reader.error || new Error("Kunde inte lasa bild"));
    reader.readAsDataURL(file);
  });
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const prompt = promptEl.value.trim();
  const images = pendingImages.slice();
  if (!prompt && images.length === 0) {
    return;
  }
  promptEl.value = "";
  pendingImages = [];
  renderImagePreview();
  form.querySelector("button[type='submit']").disabled = true;
  try {
    const imageRequest = parseImageRequest(prompt);
    if (images.length === 0 && imageRequest) {
      await generateImage(imageRequest.prompt, imageRequest.displayText);
    } else {
      await sendPrompt(prompt, images);
    }
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  } finally {
    form.querySelector("button[type='submit']").disabled = false;
    promptEl.focus();
  }
});

conversationListEl.addEventListener("click", async (event) => {
  const deleteButton = event.target.closest("[data-delete-conversation-id]");
  const item = event.target.closest("[data-conversation-id]");
  if (!item) {
    return;
  }
  try {
    if (deleteButton) {
      await confirmAndDeleteConversation(deleteButton.dataset.deleteConversationId);
      return;
    }
    await openConversation(item.dataset.conversationId);
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  }
});

conversationListEl.addEventListener("contextmenu", async (event) => {
  const item = event.target.closest("[data-conversation-id]");
  if (!item) {
    return;
  }
  event.preventDefault();
  try {
    await confirmAndDeleteConversation(item.dataset.conversationId);
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  }
});

conversationListEl.addEventListener("keydown", async (event) => {
  if (!["Delete", "Backspace", "Enter", " "].includes(event.key)) {
    return;
  }
  const item = event.target.closest("[data-conversation-id]");
  if (!item) {
    return;
  }
  event.preventDefault();
  try {
    if (event.key === "Delete" || event.key === "Backspace") {
      await confirmAndDeleteConversation(item.dataset.conversationId);
      return;
    }
    await openConversation(item.dataset.conversationId);
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  }
});

newChatButton.addEventListener("click", startNewConversation);

function parseImageRequest(prompt) {
  const trimmed = prompt.trim();
  for (const prefix of ["/bild ", "/image "]) {
    if (trimmed.toLowerCase().startsWith(prefix)) {
      const imagePrompt = trimmed.slice(prefix.length).trim();
      return imagePrompt ? { prompt: imagePrompt, displayText: trimmed } : null;
    }
  }
  if (!looksLikeImageGenerationRequest(trimmed)) {
    return null;
  }
  return { prompt: trimmed, displayText: trimmed };
}

function looksLikeImageGenerationRequest(prompt) {
  const normalized = normalizeIntentText(prompt);
  if (!normalized) {
    return false;
  }
  const directPrefixes = [
    "generera en bild",
    "skapa en bild",
    "gora en bild",
    "gor en bild",
    "gör en bild",
    "rita ",
    "teckna ",
    "make an image",
    "generate an image",
    "create an image",
    "draw ",
  ];
  if (directPrefixes.some((prefix) => normalized.startsWith(normalizeIntentText(prefix)))) {
    return true;
  }
  const conversationalPatterns = [
    /\b(?:gor|gora|skapa|generera|rita|teckna)\b.{0,48}\b(?:bild|png|illustration|teckning)\b/,
    /\b(?:jag vill ha|kan du fixa|kan du gora|kan du skapa)\b.{0,48}\b(?:bild|png|illustration|teckning)\b/,
    /\b(?:make|generate|create|draw)\b.{0,48}\b(?:image|picture|png|illustration)\b/,
  ];
  return conversationalPatterns.some((pattern) => pattern.test(normalized));
}

function normalizeIntentText(value) {
  return value
    .toLowerCase()
    .normalize("NFD")
    .replace(/[\u0300-\u036f]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

promptEl.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    form.requestSubmit();
  }
});

promptEl.addEventListener("paste", async (event) => {
  const files = [...(event.clipboardData?.files || [])].filter((file) => file.type.startsWith("image/"));
  if (files.length === 0) {
    return;
  }
  event.preventDefault();
  await Promise.all(files.map(addImageFile));
});

form.addEventListener("dragover", (event) => {
  if ([...(event.dataTransfer?.items || [])].some((item) => item.type.startsWith("image/"))) {
    event.preventDefault();
  }
});

form.addEventListener("drop", async (event) => {
  const files = [...(event.dataTransfer?.files || [])].filter((file) => file.type.startsWith("image/"));
  if (files.length === 0) {
    return;
  }
  event.preventDefault();
  await Promise.all(files.map(addImageFile));
  promptEl.focus();
});

voiceToggle.addEventListener("click", async () => {
  const previousTTS = ttsEnabled;
  const previousServerVoice = serverVoiceEnabled;
  ttsEnabled = !ttsEnabled;
  if (!ttsEnabled) {
    serverVoiceEnabled = false;
  }
  renderVoiceToggles();
  try {
    await saveRuntimeSettings();
  } catch (error) {
    ttsEnabled = previousTTS;
    serverVoiceEnabled = previousServerVoice;
    renderVoiceToggles();
    addMessage("assistant", `Fel: ${error.message}`);
  }
});

serverVoiceToggle.addEventListener("click", async () => {
  const previousTTS = ttsEnabled;
  const previousServerVoice = serverVoiceEnabled;
  serverVoiceEnabled = !serverVoiceEnabled;
  if (serverVoiceEnabled && !ttsEnabled) {
    ttsEnabled = true;
  }
  renderVoiceToggles();
  try {
    await saveRuntimeSettings();
  } catch (error) {
    ttsEnabled = previousTTS;
    serverVoiceEnabled = previousServerVoice;
    renderVoiceToggles();
    addMessage("assistant", `Fel: ${error.message}`);
  }
});

settingsButton.addEventListener("click", () => {
  renderSettingsForm();
  settingsDialog.showModal();
});

imageModelSelect.addEventListener("change", () => {
  if (imageModelSelect.value !== "sensenova-u1-8b") {
    imageLoraSelect.value = "none";
  }
  imageLoraSelect.disabled = imageModelSelect.value !== "sensenova-u1-8b";
});

settingsCloseButton.addEventListener("click", () => {
  settingsDialog.close();
});

settingsForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  settingsSaveButton.disabled = true;
  try {
    await saveSettings();
    settingsDialog.close();
  } catch (error) {
    addMessage("assistant", `Fel: ${error.message}`);
  } finally {
    settingsSaveButton.disabled = false;
  }
});

function setupSpeechRecognition() {
  const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (!SpeechRecognition) {
    micButton.disabled = true;
    micButton.title = "Voice input stods inte i den har browsern";
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
loadSettings().catch((error) => {
  statusEl.textContent = `Settings offline: ${error.message}`;
});
loadConversationList().catch((error) => {
  statusEl.textContent = `Historik offline: ${error.message}`;
});
renderConversation();
