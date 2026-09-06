document.addEventListener('DOMContentLoaded', () => {
    const API_BASE = '/api';

    // UI Elements
    const navItems = document.querySelectorAll('.nav-item');
    const tabPanes = document.querySelectorAll('.tab-pane');
    const pageTitle = document.getElementById('page-title');
    const repoSelect = document.getElementById('repo-select');

    const addRepoModal = document.getElementById('add-repo-modal');
    const btnOpenAddModal = document.getElementById('btn-open-add-modal');
    const btnCloseModal = document.getElementById('btn-close-modal');
    const btnCancelModal = document.getElementById('btn-cancel-modal');
    const addRepoForm = document.getElementById('add-repo-form');
    const modalError = document.getElementById('modal-error');

    let currentRepoID = '';

    const tabTitles = {
        overview: 'Workspace Repository Overview',
        checkpoints: 'Entire Checkpoints Log',
        requirements: 'Requirements & Milestones Matrix',
        graph: 'Entire Graph Structural Impact Analysis',
        handoff: 'Agent & Developer Handoff Package'
    };

    // Navigation Tab Switching
    navItems.forEach(item => {
        item.addEventListener('click', () => {
            const targetTab = item.getAttribute('data-tab');

            navItems.forEach(i => i.classList.remove('active'));
            tabPanes.forEach(p => p.classList.remove('active'));

            item.classList.add('active');
            const activePane = document.getElementById(`tab-${targetTab}`);
            if (activePane) activePane.classList.add('active');

            if (pageTitle && tabTitles[targetTab]) {
                pageTitle.textContent = tabTitles[targetTab];
            }
        });
    });

    // Modal Control
    if (btnOpenAddModal) {
        btnOpenAddModal.addEventListener('click', () => {
            modalError.style.display = 'none';
            addRepoForm.reset();
            addRepoModal.classList.add('active');
        });
    }

    const closeModal = () => addRepoModal.classList.remove('active');
    if (btnCloseModal) btnCloseModal.addEventListener('click', closeModal);
    if (btnCancelModal) btnCancelModal.addEventListener('click', closeModal);

    // Handle Add Repository Form Submission
    if (addRepoForm) {
        addRepoForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            modalError.style.display = 'none';

            const urlInput = document.getElementById('repo-url-input').value.trim();
            const localPathInput = document.getElementById('local-path-input').value.trim();

            try {
                const res = await fetch(`${API_BASE}/repositories`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ url: urlInput, local_path: localPathInput })
                });

                const data = await res.json();
                if (!res.ok) {
                    modalError.textContent = data.error ? data.error.message : 'Failed to add repository';
                    modalError.style.display = 'block';
                    return;
                }

                closeModal();
                await fetchRepositoriesList();
                if (data.id) {
                    await selectActiveRepository(data.id);
                }
            } catch (err) {
                modalError.textContent = 'Network or server error occurred';
                modalError.style.display = 'block';
            }
        });
    }

    // Repository Dropdown Selection Change
    if (repoSelect) {
        repoSelect.addEventListener('change', async (e) => {
            const selectedID = e.target.value;
            if (selectedID && selectedID !== currentRepoID) {
                await selectActiveRepository(selectedID);
            }
        });
    }

    // Fetch Initial Data
    fetchHealthStatus();
    fetchRepositoriesList();

    async function fetchHealthStatus() {
        try {
            const res = await fetch(`${API_BASE}/health`);
            const data = await res.json();
            const el = document.getElementById('health-status');
            if (el && data.status === 'ok') {
                el.textContent = `Online • ${data.service}`;
                el.style.color = '#10b981';
            }
        } catch (err) {
            const el = document.getElementById('health-status');
            if (el) el.textContent = 'API Unavailable';
        }
    }

    async function fetchRepositoriesList() {
        try {
            const res = await fetch(`${API_BASE}/repositories`);
            const repos = await res.json();

            if (repoSelect) {
                repoSelect.innerHTML = repos.map(r => `
                    <option value="${r.id}" ${r.is_active ? 'selected' : ''}>
                        ${r.owner}/${r.name}
                    </option>
                `).join('');
            }

            const activeRepo = repos.find(r => r.is_active) || repos[0];
            if (activeRepo) {
                currentRepoID = activeRepo.id;
                renderActiveRepoWorkspace(activeRepo);
            }
        } catch (err) {
            console.error('Failed to list repositories:', err);
        }
    }

    async function selectActiveRepository(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/select`, { method: 'POST' });
            const activeRepo = await res.json();

            currentRepoID = activeRepo.id;
            if (repoSelect) repoSelect.value = currentRepoID;

            renderActiveRepoWorkspace(activeRepo);
        } catch (err) {
            console.error('Failed to select repository:', err);
        }
    }

    async function renderActiveRepoWorkspace(repo) {
        // Details Card
        const detailsContainer = document.getElementById('repo-details');
        if (detailsContainer) {
            detailsContainer.innerHTML = `
                <div><strong>Repository Name:</strong> ${repo.name}</div>
                <div><strong>Owner:</strong> ${repo.owner}</div>
                <div><strong>GitHub URL:</strong> <a href="${repo.url}" target="_blank" style="color:#60a5fa;">${repo.url}</a></div>
                <div><strong>Default Branch:</strong> <code>${repo.default_branch}</code></div>
                <div><strong>Local Workspace Path:</strong> <code>${repo.local_path}</code></div>
                <div><strong>Description:</strong> ${repo.description}</div>
            `;
        }

        // Fetch Integration Status
        fetchIntegrationStatus(repo.id);

        // Refresh Sub-resources
        fetchCheckpoints(repo.id);
        fetchRequirements(repo.id);
        fetchGraphData(repo.id);
        fetchHandoffData(repo.id);
    }

    async function fetchIntegrationStatus(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/status`);
            const status = await res.json();
            const grid = document.getElementById('integration-status-grid');
            if (grid) {
                grid.innerHTML = `
                    <div class="readiness-card">
                        <div class="readiness-header">
                            <span>🐙 Git Executable</span>
                            <span class="status-chip ${status.git_status}">${status.git_status.toUpperCase()}</span>
                        </div>
                        <div class="readiness-desc">${status.git_message}</div>
                    </div>
                    <div class="readiness-card">
                        <div class="readiness-header">
                            <span>🐙 GitHub Remote</span>
                            <span class="status-chip ${status.github_status}">${status.github_status.toUpperCase()}</span>
                        </div>
                        <div class="readiness-desc">${status.github_message}</div>
                    </div>
                    <div class="readiness-card">
                        <div class="readiness-header">
                            <span>⚡ Entire Checkpoints</span>
                            <span class="status-chip ${status.entire_status}">${status.entire_status.toUpperCase()}</span>
                        </div>
                        <div class="readiness-desc">${status.entire_message}</div>
                    </div>
                    <div class="readiness-card">
                        <div class="readiness-header">
                            <span>🕸️ Entire Graph</span>
                            <span class="status-chip ${status.graph_status}">${status.graph_status.toUpperCase()}</span>
                        </div>
                        <div class="readiness-desc">${status.graph_message}</div>
                    </div>
                `;
            }
        } catch (err) {
            console.error('Failed to fetch integration status:', err);
        }
    }

    async function fetchCheckpoints(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/checkpoints`);
            const cps = await res.json();
            const tbody = document.getElementById('checkpoints-table-body');
            const countEl = document.getElementById('checkpoint-count');
            if (countEl) countEl.textContent = cps.length;

            if (tbody) {
                tbody.innerHTML = cps.map(cp => `
                    <tr>
                        <td><code>${cp.checkpoint_id}</code></td>
                        <td><code>${cp.commit_ref}</code></td>
                        <td>${new Date(cp.timestamp).toLocaleString()}</td>
                        <td>${cp.intent_context}</td>
                        <td><span class="status-badge completed">VERIFIED</span></td>
                    </tr>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch checkpoints:', err);
        }
    }

    async function fetchRequirements(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/requirements`);
            const reqs = await res.json();
            const tbody = document.getElementById('requirements-table-body');
            if (tbody) {
                tbody.innerHTML = reqs.map(r => `
                    <tr>
                        <td><code>${r.id}</code></td>
                        <td><strong>${r.title}</strong><br><small>${r.description}</small></td>
                        <td><span class="status-badge ${r.status}">${r.status.toUpperCase()}</span></td>
                        <td>${r.related_checkpoints.map(c => `<code>${c}</code>`).join(', ')}</td>
                        <td>${r.verification_evidence}</td>
                    </tr>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch requirements:', err);
        }
    }

    async function fetchGraphData(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/graph`);
            const findings = await res.json();
            const container = document.getElementById('graph-findings-list');
            if (container) {
                container.innerHTML = findings.map(f => `
                    <div style="margin-bottom: 16px;">
                        <h4>${f.id}: ${f.query_change}</h4>
                        <p style="margin-top: 4px; color: var(--text-secondary);">Affected Files: ${f.affected_files.join(', ')}</p>
                        <p style="color: var(--text-secondary);">Functions: ${f.affected_functions.join(', ')}</p>
                        <p style="color: var(--accent-cyan);">Evidence: ${f.source_evidence}</p>
                    </div>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch graph data:', err);
        }
    }

    async function fetchHandoffData(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/handoff`);
            const h = await res.json();
            const container = document.getElementById('handoff-content');
            if (container) {
                container.innerHTML = `
                    <p><strong>Original Intent:</strong> ${h.original_intent}</p>
                    <br>
                    <h4>Completed Work:</h4>
                    <ul>${h.completed_work.map(w => `<li>${w}</li>`).join('')}</ul>
                    <br>
                    <h4>Remaining Tasks:</h4>
                    <ul>${h.remaining_work.map(r => `<li>${r}</li>`).join('')}</ul>
                    <br>
                    <p><strong>Recommended Action:</strong> ${h.recommended_next_action}</p>
                `;
            }
        } catch (err) {
            console.error('Failed to fetch handoff:', err);
        }
    }
});
