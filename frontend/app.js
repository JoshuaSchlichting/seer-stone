import { DatabaseService } from "./bindings/github.com/seer-stone/seer-stone/index.js";

const $ = (selector) => document.querySelector(selector);
const list = $("#connection-list");
const modal = $("#connection-modal");
const passwordInput = $("#db-password");
const databaseSelect = $("#database-select");
const toast = $("#toast");
const themeSelect = $("#theme-select");
const paletteSelect = $("#palette-select");
const systemTheme = window.matchMedia("(prefers-color-scheme: dark)");
function applyTheme(preference, persist = false) {
  const dark = preference === "dark" || (preference === "system" && systemTheme.matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  themeSelect.value = preference;
  if (persist) {
    try { localStorage.setItem("seer-stone-theme", preference); } catch {}
  }
}
let savedTheme = "system";
try {
  const storedTheme = localStorage.getItem("seer-stone-theme");
  if (["system", "light", "dark"].includes(storedTheme)) savedTheme = storedTheme;
} catch {}
applyTheme(savedTheme);
themeSelect.addEventListener("change", () => applyTheme(themeSelect.value, true));
systemTheme.addEventListener("change", () => {
  if (themeSelect.value === "system") applyTheme("system");
});
const palettes = ["teal", "ocean", "violet", "ember", "rose", "forest"];
let savedPalette = "teal";
try {
  const storedPalette = localStorage.getItem("seer-stone-palette");
  if (palettes.includes(storedPalette)) savedPalette = storedPalette;
} catch {}
document.documentElement.dataset.palette = savedPalette;
paletteSelect.value = savedPalette;
paletteSelect.addEventListener("change", () => {
  document.documentElement.dataset.palette = paletteSelect.value;
  try { localStorage.setItem("seer-stone-palette", paletteSelect.value); } catch {}
});
let profiles = [];
let selectedId = "";
const connectedIds = new Set();
let schemaData = [];
const availableDatabases = new Map();
const selectedDatabases = new Map();
const expandedSchemas = new Set();
const expandedObjects = new Set();
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

function engineInfo(engine) {
  switch (engine || "cockroach") {
    case "postgres": return { name: "PostgreSQL", icon: "PG" };
    case "snowflake": return { name: "Snowflake", icon: "SF" };
    default: return { name: "CockroachDB", icon: "CR" };
  }
}

function updateEngineFields() {
  const engine = $("#profile-engine").value;
  const snowflake = engine === "snowflake";
  $("#snowflake-fields").classList.toggle("is-hidden", !snowflake);
  $("#profile-port-field").classList.toggle("is-hidden", snowflake);
  $("#profile-ssl-field").classList.toggle("is-hidden", snowflake);
  $("#profile-database").required = !snowflake;
  $("#profile-host-label").textContent = snowflake ? "Account identifier" : "Host";
  $("#profile-host").placeholder = snowflake ? "organization-account.region" : (engine === "postgres" ? "db.example.com" : "cluster-name.gcp-us-east1.cockroachlabs.cloud");
  $("#profile-database").value = snowflake ? "" : ($("#profile-database").value || "defaultdb");
  $("#profile-port").value = engine === "postgres" ? "5432" : "26257";
}

function renderProfiles() {
  list.replaceChildren();
  $("#empty-connections").classList.toggle("is-hidden", profiles.length > 0);
  for (const profile of profiles) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `connection-item${profile.id === selectedId ? " selected" : ""}`;
    button.dataset.id = profile.id;
    const engine = engineInfo(profile.engine);
    button.innerHTML = `<span class="connection-symbol">${engine.icon}</span><span class="connection-copy"><strong></strong><small></small></span><span class="item-dot${connectedIds.has(profile.id) ? " connected" : ""}"></span>`;
    button.querySelector("strong").textContent = profile.name;
    button.querySelector("small").textContent = `${profile.database || "Select database"} · ${profile.host}`;
    list.append(button);
  }
  const profile = activeProfile();
  $("#active-name").textContent = profile?.name || "Select a cluster";
  const engine = engineInfo(profile?.engine);
  $("#target-label").textContent = profile ? `${engine.name} · ${profile.username}@${profile.host}${profile.port ? `:${profile.port}` : ""}${profile.database ? `/${profile.database}` : ""}` : "Choose a connection from the sidebar";
  $("#engine-badge").innerHTML = profile ? `<span>${engine.icon}</span> ${engine.name}` : '<span>SQL</span> SQL database';
  passwordInput.disabled = !profile;
  if (profile) passwordInput.value = passwords.get(profile.id) || "";
  $("#connect-button").disabled = !profile;
  $("#run-query").disabled = !profile;
  $("#refresh-schema").disabled = !profile || !connectedIds.has(profile.id);
  $("#schema-filter").disabled = !profile;
  databaseSelect.disabled = !profile || !connectedIds.has(profile.id);
  databaseSelect.replaceChildren();
  const databases = profile ? (availableDatabases.get(profile.id) || []) : [];
  if (databases.length) {
    for (const name of databases) {
      const option = document.createElement("option");
      option.value = name;
      option.textContent = name;
      databaseSelect.append(option);
    }
    const chosen = selectedDatabases.get(profile.id) || profile.database;
    databaseSelect.value = databases.includes(chosen) ? chosen : databases[0];
  } else {
    const option = document.createElement("option");
    option.value = "";
    option.textContent = profile ? "Connect to load databases" : "Select a connection";
    databaseSelect.append(option);
  }
  const isConnected = Boolean(profile && connectedIds.has(profile.id));
  $("#connection-state").textContent = isConnected ? "● Connected" : "● Disconnected";
  $("#connection-state").classList.toggle("is-connected", isConnected);
  $("#connect-button").textContent = isConnected ? "Reconnect" : "Connect";
}

function openModal() {
  $("#connection-form").reset();
  $("#profile-id").value = "";
  $("#profile-engine").value = "cockroach";
  $("#profile-port").value = "26257";
  $("#profile-database").value = "defaultdb";
  $("#profile-warehouse").value = "";
  $("#profile-schema").value = "";
  $("#profile-role").value = "";
  $("#profile-ssl").value = "verify-full";
  updateEngineFields();
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

function renderSchemaTree(message = "") {
  const tree = $("#schema-tree");
  tree.replaceChildren();
  const filter = $("#schema-filter").value.trim().toLowerCase();
  const toggleSchemasButton = $("#toggle-schemas");
  const allSchemasExpanded = schemaData.length > 0 && schemaData.every((schema) => expandedSchemas.has(schema.name));
  toggleSchemasButton.disabled = schemaData.length === 0;
  toggleSchemasButton.textContent = allSchemasExpanded ? "Collapse all" : "Expand all";
  toggleSchemasButton.title = allSchemasExpanded ? "Collapse all schemas" : "Expand all schemas";
  if (!schemaData.length) {
    const empty = document.createElement("div");
    empty.className = "schema-empty";
    empty.textContent = message || "No schemas found.";
    tree.append(empty);
    return;
  }

  for (const schema of schemaData) {
    const objects = schema.objects.filter((object) => {
      if (!filter) return true;
      return `${schema.name} ${object.name} ${object.columns.map((column) => column.name).join(" ")}`.toLowerCase().includes(filter);
    });
    if (filter && !objects.length && !schema.name.toLowerCase().includes(filter)) continue;
    const node = document.createElement("div");
    node.className = "schema-node";
    const schemaButton = document.createElement("button");
    schemaButton.className = "schema-node-button";
    const isOpen = expandedSchemas.has(schema.name) || Boolean(filter);
    schemaButton.innerHTML = `<span class="schema-chevron">${isOpen ? "▾" : "▸"}</span><span class="schema-icon">▱</span><span class="schema-node-name"></span><span class="schema-count"></span>`;
    schemaButton.querySelector(".schema-node-name").textContent = schema.name;
    schemaButton.querySelector(".schema-count").textContent = String(objects.length);
    schemaButton.addEventListener("click", () => {
      if (expandedSchemas.has(schema.name)) expandedSchemas.delete(schema.name);
      else expandedSchemas.add(schema.name);
      renderSchemaTree();
    });
    node.append(schemaButton);

    if (isOpen) {
      const children = document.createElement("div");
      children.className = "schema-children";
      for (const object of objects) {
        const key = `${schema.name}.${object.name}`;
        const objectNode = document.createElement("div");
        const objectOpen = expandedObjects.has(key) || Boolean(filter);
        const objectRow = document.createElement("div");
        objectRow.className = "schema-object-row";
        const objectButton = document.createElement("button");
        objectButton.className = "schema-object-button";
        const kindIcon = object.kind.includes("view") ? "◉" : "▦";
        objectButton.innerHTML = `<span class="object-kind">${kindIcon}</span><span class="object-name"></span>`;
        objectButton.querySelector(".object-name").textContent = object.name;
        objectButton.title = `Preview ${object.kind} · ${object.columns.length} columns`;
        objectButton.addEventListener("click", () => {
          const query = `SELECT * FROM ${quoteIdentifier(schema.name)}.${quoteIdentifier(object.name)} LIMIT 50;`;
          $("#sql-editor").value = query;
          executeQuery(query, `${schema.name}.${object.name}`);
        });
        const expandButton = document.createElement("button");
        expandButton.className = "schema-expand-button";
        expandButton.type = "button";
        expandButton.setAttribute("aria-label", `${objectOpen ? "Hide" : "Show"} columns for ${object.name}`);
        expandButton.textContent = objectOpen ? "▾" : "▸";
        expandButton.addEventListener("click", () => {
          if (expandedObjects.has(key)) expandedObjects.delete(key);
          else expandedObjects.add(key);
          renderSchemaTree();
        });
        objectRow.append(objectButton, expandButton);
        objectNode.append(objectRow);
        if (objectOpen) {
          const columns = document.createElement("div");
          columns.className = "column-list";
          const visibleColumns = object.columns.filter((column) => !filter || `${column.name} ${column.dataType}`.toLowerCase().includes(filter));
          for (const column of visibleColumns) {
            const item = document.createElement("div");
            item.className = "column-item";
            const name = document.createElement("span");
            name.className = "column-name";
            name.textContent = column.name;
            name.title = column.name;
            const meta = document.createElement("span");
            meta.className = "column-meta";
            meta.textContent = `${column.dataType}${column.nullable ? " · nullable" : " · not null"}${column.default ? " · default" : ""}`;
            meta.title = column.default || "";
            item.append(name, meta);
            columns.append(item);
          }
          if (!visibleColumns.length) {
            const noColumns = document.createElement("div");
            noColumns.className = "schema-empty-filter";
            noColumns.textContent = "No matching columns";
            columns.append(noColumns);
          }
          objectNode.append(columns);
        }
        children.append(objectNode);
      }
      node.append(children);
    }
    tree.append(node);
  }
  if (!tree.children.length) {
    const empty = document.createElement("div");
    empty.className = "schema-empty";
    empty.textContent = "No matching tables or views.";
    tree.append(empty);
  }
}

async function refreshSchema() {
  const profile = activeProfile();
  if (!profile) return;
  const password = passwordInput.value || passwords.get(profile.id) || "";
  if (!password) {
    showToast("Enter the database password before exploring schema.", true);
    passwordInput.focus();
    return;
  }
  const refreshButton = $("#refresh-schema");
  refreshButton.classList.add("schema-refreshing");
  $("#schema-tree").innerHTML = '<div class="schema-status">Loading schemas, tables, views, and columns…</div>';
  try {
    schemaData = await DatabaseService.ExploreSchema(profile.id, password, databaseSelect.value);
    expandedSchemas.clear();
    expandedObjects.clear();
    renderSchemaTree();
    const objectCount = schemaData.reduce((count, schema) => count + schema.objects.length, 0);
    showToast(`Schema loaded · ${schemaData.length} schemas, ${objectCount} tables and views`);
  } catch (error) {
    schemaData = [];
    renderSchemaTree(`Schema load failed: ${error}`);
    showToast(`Could not explore schema: ${error}`, true);
  } finally {
    refreshButton.classList.remove("schema-refreshing");
  }
}

$("#refresh-schema").addEventListener("click", refreshSchema);
$("#toggle-schemas").addEventListener("click", () => {
  if (!schemaData.length) return;
  const allExpanded = schemaData.every((schema) => expandedSchemas.has(schema.name));
  expandedSchemas.clear();
  if (!allExpanded) {
    for (const schema of schemaData) expandedSchemas.add(schema.name);
  }
  $("#schema-filter").value = "";
  renderSchemaTree();
});
databaseSelect.addEventListener("change", () => {
  const profile = activeProfile();
  if (!profile) return;
  selectedDatabases.set(profile.id, databaseSelect.value);
  refreshSchema();
});
$("#schema-filter").addEventListener("input", () => renderSchemaTree());

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

function selectConnection(id) {
  if (selectedId === id) return;
  schemaData = [];
  expandedSchemas.clear();
  expandedObjects.clear();
  renderSchemaTree("Select Connect to load schema metadata.");
  selectedId = id;
  renderProfiles();
  $("#query-message").textContent = "Ready to query";
  if (connectedIds.has(id)) refreshSchema();
}

list.addEventListener("click", (event) => {
  const item = event.target.closest(".connection-item");
  if (item) selectConnection(item.dataset.id);
});

list.addEventListener("dblclick", (event) => {
  const item = event.target.closest(".connection-item");
  if (!item) return;
  selectConnection(item.dataset.id);
  connectSelected();
});

$("#profile-engine").addEventListener("change", updateEngineFields);
$("#add-connection").addEventListener("click", openModal);
$("#close-modal").addEventListener("click", closeModal);
$("#cancel-modal").addEventListener("click", closeModal);
$(".modal-background").addEventListener("click", closeModal);

passwordInput.addEventListener("input", () => {
  if (selectedId) passwords.set(selectedId, passwordInput.value);
  connectedIds.delete(selectedId);
  availableDatabases.delete(selectedId);
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
      engine: $("#profile-engine").value,
      host: $("#profile-host").value,
      port: $("#profile-port").value,
      database: $("#profile-database").value,
      schema: $("#profile-schema").value,
      warehouse: $("#profile-warehouse").value,
      role: $("#profile-role").value,
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

async function connectSelected() {
  if (!selectedId) return;
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
    const databases = await DatabaseService.Databases(selectedId, password);
    connectedIds.add(selectedId);
    availableDatabases.set(selectedId, databases);
    const current = selectedDatabases.get(selectedId) || activeProfile()?.database;
    selectedDatabases.set(selectedId, databases.includes(current) ? current : (databases[0] || activeProfile()?.database || ""));
    renderProfiles();
    await refreshSchema();
    $("#query-message").textContent = "Connected successfully";
    showToast(`Connected to ${engineInfo(activeProfile()?.engine).name} · ${serverVersion.split(" ").slice(0, 2).join(" ")}`);
  } catch (error) {
    connectedIds.delete(selectedId);
    availableDatabases.delete(selectedId);
    renderProfiles();
    $("#query-message").textContent = "Connection failed";
    showToast(String(error), true);
  } finally {
    button.classList.remove("is-loading");
  }
}

$("#connect-button").addEventListener("click", connectSelected);

async function executeQuery(query, source = "query") {
  if (!selectedId) return;
  const password = passwordInput.value || passwords.get(selectedId) || "";
  if (!password) {
    showToast("Enter the database password first.", true);
    passwordInput.focus();
    return;
  }
  passwords.set(selectedId, password);
  const button = $("#run-query");
  button.classList.add("is-loading");
  $("#query-message").textContent = source === "query" ? "Running query…" : `Loading ${source}…`;
  try {
    const result = await DatabaseService.RunQuery(selectedId, password, databaseSelect.value, query);
    connectedIds.add(selectedId);
    renderResult(result);
    renderProfiles();
    $("#query-message").textContent = source === "query" ? "Query completed" : `Preview: ${source}`;
    if (source !== "query") $(".results-section").scrollIntoView({ behavior: "smooth", block: "nearest" });
  } catch (error) {
    $("#query-message").textContent = "Query failed";
    showToast(String(error), true);
  } finally {
    button.classList.remove("is-loading");
  }
}

function quoteIdentifier(identifier) {
  return `"${identifier.replaceAll('"', '""')}"`;
}

$("#run-query").addEventListener("click", () => executeQuery($("#sql-editor").value));

$("#sql-editor").addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
    event.preventDefault();
    $("#run-query").click();
  }
});

refreshProfiles();
