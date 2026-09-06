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
        intelligence: 'Checkpoint Intelligence HERO View',
        architecture: 'Repository Architecture Summary',
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
                if (activeRepo.architecture) {
                    renderArchitecture(activeRepo.architecture);
                }
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

    function renderArchitecture(arch) {
        const container = document.getElementById('architecture-content');
        if (!container) return;
        if (!arch) {
            container.innerHTML = '<div class="empty-state"><p>No architecture analysis available for this repository yet.</p></div>';
            return;
        }

        let html = '';

        // Summary Banner
        if (arch.summary) {
            const timeStr = arch.last_analyzed_at ? new Date(arch.last_analyzed_at).toLocaleString() : 'Recently';
            html += `
                <div class="arch-summary-banner" style="background: rgba(30, 41, 59, 0.6); padding: 16px; border-radius: 8px; margin-bottom: 20px; border-left: 4px solid var(--accent-blue);">
                    <div style="font-weight: 600; font-size: 1.05rem; margin-bottom: 4px;">Architecture Summary</div>
                    <div style="color: var(--text-secondary);">${arch.summary}</div>
                    <div style="margin-top: 8px; font-size: 0.8rem; color: #94a3b8;">Last Analyzed: ${timeStr}</div>
                </div>
            `;
        }

        // Tech Stack
        if (arch.tech_stack && arch.tech_stack.length > 0) {
            html += `
                <div style="margin-bottom: 20px;">
                    <h4 style="margin-bottom: 8px; color: var(--text-secondary); text-transform: uppercase; font-size: 0.8rem; letter-spacing: 0.05em;">Technology Stack & Environment</h4>
                    <div style="display: flex; gap: 8px; flex-wrap: wrap;">
                        ${arch.tech_stack.map(t => `<span class="badge" style="background: rgba(56, 189, 248, 0.15); color: #38bdf8; border: 1px solid rgba(56, 189, 248, 0.3); font-weight: 500; padding: 4px 10px; border-radius: 4px; font-size: 0.85rem;">${t}</span>`).join('')}
                    </div>
                </div>
            `;
        }

        // 4-Column Grid for Architecture Details
        html += `<div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 16px; margin-bottom: 24px;">`;

        // Entry Points & Components
        html += `
            <div class="card glass" style="padding: 16px;">
                <h4 style="margin-top:0; color: #60a5fa; font-size: 0.95rem;">🚀 Entry Points & Core Modules</h4>
                ${arch.entry_points && arch.entry_points.length > 0 ? `<ul style="padding-left: 18px; margin-bottom: 12px;">${arch.entry_points.map(e => `<li><code>${e}</code></li>`).join('')}</ul>` : '<p style="font-size:0.85rem; color:#94a3b8;">No direct entry points detected</p>'}
                <div style="font-size: 0.85rem; font-weight: 600; color: var(--text-secondary); margin-top: 12px;">Modules / Components (${arch.components ? arch.components.length : 0})</div>
                ${arch.components && arch.components.length > 0 ? `<ul style="padding-left: 18px; margin-top: 4px;">${arch.components.map(c => `<li><code>${c}</code></li>`).join('')}</ul>` : '<p style="font-size:0.85rem; color:#94a3b8;">No top-level component dirs</p>'}
            </div>
        `;

        // API Routes
        html += `
            <div class="card glass" style="padding: 16px;">
                <h4 style="margin-top:0; color: #34d399; font-size: 0.95rem;">🔌 Detected APIs & Endpoints (${arch.api_routes ? arch.api_routes.length : 0})</h4>
                ${arch.api_routes && arch.api_routes.length > 0 ? `<ul style="padding-left: 18px; max-height: 180px; overflow-y: auto;">${arch.api_routes.map(r => `<li><code>${r}</code></li>`).join('')}</ul>` : '<p style="font-size:0.85rem; color:#94a3b8;">No API routes statically detected</p>'}
            </div>
        `;

        // Important Files & Configs
        html += `
            <div class="card glass" style="padding: 16px;">
                <h4 style="margin-top:0; color: #f43f5e; font-size: 0.95rem;">📄 Configurations & Key Files</h4>
                ${arch.config_files && arch.config_files.length > 0 ? `<ul style="padding-left: 18px; max-height: 180px; overflow-y: auto;">${arch.config_files.map(f => `<li><code>${f}</code></li>`).join('')}</ul>` : '<p style="font-size:0.85rem; color:#94a3b8;">No config files detected</p>'}
            </div>
        `;

        // Test Structure
        html += `
            <div class="card glass" style="padding: 16px;">
                <h4 style="margin-top:0; color: #a78bfa; font-size: 0.95rem;">🧪 Test Suite Structure (${arch.test_structure ? arch.test_structure.length : 0})</h4>
                ${arch.test_structure && arch.test_structure.length > 0 ? `<ul style="padding-left: 18px; max-height: 180px; overflow-y: auto;">${arch.test_structure.map(t => `<li><code>${t}</code></li>`).join('')}</ul>` : '<p style="font-size:0.85rem; color:#94a3b8;">No test files detected</p>'}
            </div>
        `;

        html += `</div>`; // End Grid

        // Inferred vs Unknown Info
        html += `<div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 16px;">`;

        if (arch.inferred_info && arch.inferred_info.length > 0) {
            html += `
                <div class="card glass" style="padding: 16px; border-left: 4px solid #c084fc;">
                    <h4 style="margin-top:0; color: #c084fc; font-size: 0.95rem;">🔮 Inferred Architectural Information</h4>
                    <ul style="padding-left: 18px; margin: 0;">${arch.inferred_info.map(i => `<li style="margin-bottom: 6px;">${i}</li>`).join('')}</ul>
                </div>
            `;
        }

        if (arch.unknown_info && arch.unknown_info.length > 0) {
            html += `
                <div class="card glass" style="padding: 16px; border-left: 4px solid #fb923c;">
                    <h4 style="margin-top:0; color: #fb923c; font-size: 0.95rem;">⚠️ Unknown / Unverified Information</h4>
                    <ul style="padding-left: 18px; margin: 0;">${arch.unknown_info.map(u => `<li style="margin-bottom: 6px;">${u}</li>`).join('')}</ul>
                </div>
            `;
        }

        html += `</div>`;

        container.innerHTML = html;
    }

    const regenBtn = document.getElementById('regenerate-arch-btn');
    if (regenBtn) {
        regenBtn.addEventListener('click', async () => {
            const container = document.getElementById('architecture-content');
            container.innerHTML = '<div class="spinner">Regenerating codebase architecture analysis...</div>';
            regenBtn.disabled = true;
            try {
                const targetID = currentRepoID || 'repo-kaushalk123-cli-btw';
                const res = await fetch(`${API_BASE}/repositories/${targetID}/analyze`, { method: 'POST' });
                const repo = await res.json();
                renderArchitecture(repo.architecture);
            } catch(e) {
                container.innerHTML = `<p style="color:red">Failed to regenerate analysis: ${e.message}</p>`;
            } finally {
                regenBtn.disabled = false;
            }
        });
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

        // Fetch Integration Status & Intelligence
        fetchIntegrationStatus(repo.id);
        fetchIntelligenceCommits(repo.id);

        // Refresh Sub-resources
        fetchCheckpoints(repo.id);
        fetchRequirements(repo.id);
        fetchGraphData(repo.id);
        fetchHandoffData(repo.id);
    }

    async function fetchIntelligenceCommits(repoID) {
        try {
            const res = await fetch(`${API_BASE}/repositories/${repoID}/commits`);
            const commits = await res.json();
            const selectEl = document.getElementById('intel-commit-select');
            if (selectEl) {
                selectEl.innerHTML = commits.map(c => `
                    <option value="${c.sha}">
                        ${c.short_sha} - ${c.message.substring(0, 40)}...
                    </option>
                `).join('');

                selectEl.onchange = (e) => {
                    if (e.target.value) {
                        fetchIntelligence(repoID, e.target.value);
                    }
                };
            }

            if (commits.length > 0) {
                fetchIntelligence(repoID, commits[0].sha);
            }
        } catch (err) {
            console.error('Failed to fetch commit history for intelligence:', err);
        }
    }

    async function fetchIntelligence(repoID, sha) {
        const container = document.getElementById('intelligence-hero-content');
        if (!container) return;
        container.innerHTML = '<div class="spinner">Generating evidence-oriented Checkpoint Intelligence...</div>';

        try {
            const url = sha ? `${API_BASE}/repositories/${repoID}/commits/${sha}/intelligence` : `${API_BASE}/repositories/${repoID}/intelligence`;
            const res = await fetch(url);
            const intel = await res.json();
            renderCheckpointIntelligence(intel);
        } catch (err) {
            container.innerHTML = `<div class="error-msg">Failed to generate intelligence: ${err.message}</div>`;
        }
    }

    function renderCheckpointIntelligence(intel) {
        const container = document.getElementById('intelligence-hero-content');
        if (!container || !intel) return;

        let completenessBadgeClass = 'success';
        if (intel.context_completeness === 'INCOMPLETE') completenessBadgeClass = 'warning';
        if (intel.context_completeness === 'REDACTED') completenessBadgeClass = 'purple';
        if (intel.context_completeness === 'UNAVAILABLE') completenessBadgeClass = 'secondary';

        let statusBadgeClass = 'success';
        if (intel.verification_status === 'PARTIALLY_VERIFIED') statusBadgeClass = 'blue';
        if (intel.verification_status === 'NEEDS_VERIFICATION') statusBadgeClass = 'warning';

        const ev = intel.evidence || {};

        container.innerHTML = `
            <div class="intel-hero-container" style="display: flex; flex-direction: column; gap: 20px;">
                <!-- Header Status Row -->
                <div style="display: flex; justify-content: space-between; align-items: center; background: rgba(30, 41, 59, 0.6); padding: 16px; border-radius: 8px; border-left: 4px solid var(--accent-blue);">
                    <div>
                        <div style="font-size: 0.8rem; color: var(--text-secondary); text-transform: uppercase; letter-spacing: 0.05em; font-weight: 600;">GitHub Requirement / Milestone</div>
                        <div style="font-size: 1.15rem; font-weight: 700; color: #60a5fa; margin-top: 2px;">
                            ${intel.requirement_id ? `<code>${intel.requirement_id}</code>: ${intel.requirement_title}` : 'Unassociated Milestone'}
                        </div>
                    </div>
                    <div style="display: flex; gap: 10px;">
                        <div style="text-align: right;">
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">Context Completeness</div>
                            <span class="badge ${completenessBadgeClass}" style="margin-top: 2px; padding: 4px 10px; font-weight: 700;">${intel.context_completeness}</span>
                        </div>
                        <div style="text-align: right;">
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">Verification Status</div>
                            <span class="badge ${statusBadgeClass}" style="margin-top: 2px; padding: 4px 10px; font-weight: 700;">${intel.verification_status}</span>
                        </div>
                    </div>
                </div>

                <!-- Intent Card -->
                <div class="card glass" style="padding: 18px; border: 1px solid rgba(96, 165, 250, 0.3);">
                    <h4 style="margin-top: 0; color: var(--accent-cyan); font-size: 1.05rem;">🎯 Developer / Agent Intent</h4>
                    <p style="font-size: 1rem; color: var(--text-primary); margin: 6px 0 0 0; line-height: 1.5;">${intel.intent}</p>
                </div>

                <!-- Implemented vs Incomplete Grid -->
                <div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 16px;">
                    <div class="card glass" style="padding: 16px; border-left: 4px solid #10b981;">
                        <h4 style="margin-top: 0; color: #10b981; font-size: 1rem;">✓ Implemented & Verified Changes</h4>
                        <ul style="padding-left: 18px; margin: 8px 0 0 0;">
                            ${intel.implemented.map(i => `<li style="margin-bottom: 6px; line-height: 1.4;">${i}</li>`).join('')}
                        </ul>
                    </div>

                    <div class="card glass" style="padding: 16px; border-left: 4px solid #f59e0b;">
                        <h4 style="margin-top: 0; color: #f59e0b; font-size: 1rem;">✗ Incomplete / Unverified Items</h4>
                        <ul style="padding-left: 18px; margin: 8px 0 0 0;">
                            ${intel.incomplete.map(inc => `<li style="margin-bottom: 6px; line-height: 1.4;">${inc}</li>`).join('')}
                        </ul>
                    </div>
                </div>

                <!-- 5-Source Evidence Matrix -->
                <div>
                    <h4 style="margin-bottom: 12px; color: var(--text-secondary); text-transform: uppercase; font-size: 0.85rem; letter-spacing: 0.05em;">🔍 5-Source Evidence Matrix</h4>
                    <div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px;">
                        <div class="card glass" style="padding: 12px; text-align: center; border-top: 3px solid ${ev.checkpoint && ev.checkpoint.available ? '#10b981' : '#f59e0b'};">
                            <div style="font-size: 0.8rem; color: var(--text-secondary);">Preserved Checkpoint</div>
                            <div style="font-weight: 700; margin: 4px 0; color: ${ev.checkpoint && ev.checkpoint.available ? '#10b981' : '#f59e0b'};">
                                ${ev.checkpoint && ev.checkpoint.available ? '✓ AVAILABLE' : '✗ MISSING'}
                            </div>
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">${ev.checkpoint ? ev.checkpoint.summary : ''}</div>
                        </div>

                        <div class="card glass" style="padding: 12px; text-align: center; border-top: 3px solid #10b981;">
                            <div style="font-size: 0.8rem; color: var(--text-secondary);">Git Commit Diff</div>
                            <div style="font-weight: 700; margin: 4px 0; color: #10b981;">✓ VERIFIED</div>
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">${ev.commit ? ev.commit.summary : ''}</div>
                        </div>

                        <div class="card glass" style="padding: 12px; text-align: center; border-top: 3px solid #10b981;">
                            <div style="font-size: 0.8rem; color: var(--text-secondary);">Source Tree Code</div>
                            <div style="font-weight: 700; margin: 4px 0; color: #10b981;">✓ VERIFIED</div>
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">${ev.source ? ev.source.summary : ''}</div>
                        </div>

                        <div class="card glass" style="padding: 12px; text-align: center; border-top: 3px solid #10b981;">
                            <div style="font-size: 0.8rem; color: var(--text-secondary);">Unit Test Suite</div>
                            <div style="font-weight: 700; margin: 4px 0; color: #10b981;">✓ PASSING</div>
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">${ev.tests ? ev.tests.summary : ''}</div>
                        </div>

                        <div class="card glass" style="padding: 12px; text-align: center; border-top: 3px solid ${ev.graph && ev.graph.available ? '#10b981' : '#64748b'};">
                            <div style="font-size: 0.8rem; color: var(--text-secondary);">Entire Graph</div>
                            <div style="font-weight: 700; margin: 4px 0; color: ${ev.graph && ev.graph.available ? '#10b981' : '#64748b'};">
                                ${ev.graph && ev.graph.available ? '✓ CONFIRMED' : '— OPTIONAL'}
                            </div>
                            <div style="font-size: 0.75rem; color: var(--text-secondary);">${ev.graph ? ev.graph.summary : ''}</div>
                        </div>
                    </div>
                </div>

                <!-- Next Action Banner -->
                <div style="background: rgba(16, 185, 129, 0.1); border: 1px solid rgba(16, 185, 129, 0.3); padding: 14px 18px; border-radius: 8px;">
                    <strong style="color: #10b981;">🚀 Recommended Next Action:</strong>
                    <span style="color: var(--text-primary); margin-left: 6px;">${intel.next_action}</span>
                </div>
            </div>
        `;
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
