import { DatabaseService } from "./bindings/github.com/seer-stone/seer-stone/index.js";

const $ = (selector) => document.querySelector(selector);
const list = $("#connection-list");
const modal = $("#connection-modal");
const passwordInput = $("#db-password");
const toast = $("#toast");
let profiles = [];
let selectedId = "";
let connectedId = "";
const passwords = new Map();

function showToast(message, isError = false) {
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.classList.add("show");
  clearTimeout(showToast.timer);
  showToast.timer = setTimeout(() => toast.classList.remove("show"), 3500);
}

function activeProfile() {
  return profiles.find((profile) => profile.id === selectedId);
}

function renderProfiles() {
  list.replaceChildren();
  $("#empty-connections").classList.toggle("is-hidden", profiles.length > 0);
  for (const profile of profiles) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `connection-item${profile.id === selectedId ? " selected" : ""}`;
    button.dataset.id = profile.id;
    button.innerHTML = `<span class="connection-symbol">CR</span><span class="connection-copy"><strong></strong><small></small></span><span class="item-dot${connectedId === profile.id ? " connected" : ""}"></span>`;
    button.querySelector("strong").textContent = profile.name;
    button.querySelector("small").textContent = `${profile.database} · ${profile.host}`;
    list.append(button);
  }
  const profile = activeProfile();
  $("#active-name").textContent = profile?.name || "Select a cluster";
  $("#target-label").textContent = profile ? `${profile.username}@${profile.host}:${profile.port}/${profile.database}` : "Choose a cluster from the sidebar";
  passwordInput.disabled = !profile;
  if (profile) passwordInput.value = passwords.get(profile.id) || "";
  $("#connect-button").disabled = !profile;
  $("#run-query").disabled = !profile;
  const isConnected = Boolean(profile && connectedId === profile.id);
  $("#connection-state").textContent = isConnected ? "● Connected" : "● Disconnected";
  $("#connection-state").classList.toggle("is-connected", isConnected);
  $("#connect-button").textContent = isConnected ? "Reconnect" : "Connect";
}

function openModal() {
  $("#connection-form").reset();
  $("#profile-id").value = "";
  $("#profile-port").value = "26257";
  $("#profile-database").value = "defaultdb";
  $("#profile-ssl").value = "verify-full";
  $("#profile-error").textContent = "";
  modal.classList.add("is-active");
  $("#profile-name").focus();
}

function closeModal() {
  modal.classList.remove("is-active");
}

function formatCell(value) {
  if (value === null || value === undefined) return '<span class="null-value">NULL</span>';
  if (typeof value === "object") return escapeHTML(JSON.stringify(value));
  return escapeHTML(String(value));
}

function escapeHTML(value) {
  return value.replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
}

function renderResult(result) {
  const wrap = $("#results-table-wrap");
  const state = $("#result-state");
  state.classList.add("is-hidden");
  wrap.classList.remove("is-hidden");
  if (!result.columns?.length) {
    wrap.innerHTML = `<div class="non-tabular-result">Statement completed successfully.</div>`;
  } else {
    const header = result.columns.map((column) => `<th>${escapeHTML(column)}</th>`).join("");
    const body = result.rows.map((row) => `<tr>${row.map((value) => `<td>${formatCell(value)}</td>`).join("")}</tr>`).join("");
    wrap.innerHTML = `<table><thead><tr>${header}</tr></thead><tbody>${body || `<tr><td colspan="${result.columns.length}" class="no-rows">No rows returned</td></tr>`}</tbody></table>`;
  }
  $("#result-meta").textContent = `${result.rowCount} row${result.rowCount === 1 ? "" : "s"} · ${result.durationMs} ms`;
}

async function refreshProfiles() {
  try {
    profiles = await DatabaseService.Profiles();
    for (const profile of profiles) {
      const password = await DatabaseService.LocalPassword(profile.id);
      if (password) passwords.set(profile.id, password);
    }
    renderProfiles();
  } catch (error) {
    showToast(`Could not load saved connections: ${error}`, true);
  }
}

list.addEventListener("click", (event) => {
  const item = event.target.closest(".connection-item");
  if (!item) return;
  selectedId = item.dataset.id;
  renderProfiles();
  $("#query-message").textContent = "Ready to query";
});

$("#add-connection").addEventListener("click", openModal);
$("#close-modal").addEventListener("click", closeModal);
$("#cancel-modal").addEventListener("click", closeModal);
$(".modal-background").addEventListener("click", closeModal);

passwordInput.addEventListener("input", () => {
  if (selectedId) passwords.set(selectedId, passwordInput.value);
  connectedId = "";
  renderProfiles();
});

$("#connection-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const saveButton = $("#save-profile");
  saveButton.classList.add("is-loading");
  $("#profile-error").textContent = "";
  try {
    const saved = await DatabaseService.SaveProfile({
      id: $("#profile-id").value,
      name: $("#profile-name").value,
      host: $("#profile-host").value,
      port: $("#profile-port").value,
      database: $("#profile-database").value,
      username: $("#profile-username").value,
      sslMode: $("#profile-ssl").value,
    });
    const password = $("#profile-password").value;
    if (password) passwords.set(saved.id, password);
    profiles = await DatabaseService.Profiles();
    selectedId = saved.id;
    renderProfiles();
    closeModal();
    showToast("Connection saved. Credentials remain in memory only.");
  } catch (error) {
    $("#profile-error").textContent = String(error);
  } finally {
    saveButton.classList.remove("is-loading");
  }
});

$("#connect-button").addEventListener("click", async () => {
  const password = passwordInput.value;
  if (!password) {
    showToast("Enter the database password first.", true);
    passwordInput.focus();
    return;
  }
  passwords.set(selectedId, password);
  const button = $("#connect-button");
  button.classList.add("is-loading");
  $("#query-message").textContent = "Connecting…";
  try {
    const serverVersion = await DatabaseService.TestConnection(selectedId, password);
    connectedId = selectedId;
    renderProfiles();
    $("#query-message").textContent = "Connected successfully";
    showToast(`Connected to CockroachDB · ${serverVersion.split(" ").slice(0, 2).join(" ")}`);
  } catch (error) {
    connectedId = "";
    renderProfiles();
    $("#query-message").textContent = "Connection failed";
    showToast(String(error), true);
  } finally {
    button.classList.remove("is-loading");
  }
});

$("#run-query").addEventListener("click", async () => {
  if (!selectedId) return;
  const password = passwordInput.value;
  if (!password) {
    showToast("Enter the database password first.", true);
    passwordInput.focus();
    return;
  }
  passwords.set(selectedId, password);
  const button = $("#run-query");
  button.classList.add("is-loading");
  $("#query-message").textContent = "Running query…";
  try {
    const result = await DatabaseService.RunQuery(selectedId, password, $("#sql-editor").value);
    connectedId = selectedId;
    renderResult(result);
    renderProfiles();
    $("#query-message").textContent = "Query completed";
  } catch (error) {
    $("#query-message").textContent = "Query failed";
    showToast(String(error), true);
  } finally {
    button.classList.remove("is-loading");
  }
});

$("#sql-editor").addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
    event.preventDefault();
    $("#run-query").click();
  }
});

refreshProfiles();
