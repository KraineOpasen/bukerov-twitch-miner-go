"""Exception types for the skill-update tooling.

The distinction that matters here is between *this tool broke* and *this update is refused*.
Only the first is an exception. A refused update -- a merge conflict, a licence change, a new
executable -- is an ordinary, expected outcome that the analyzer returns as data (a
`BlockedReason`), because it has to be rendered into an issue body and compared against
previous runs for deduplication. Modelling a refusal as an exception would make the common
case flow through error handling and would make "blocked" indistinguishable from "crashed" in
the workflow's exit status, which is exactly backwards: a blocked provider is the bot working
correctly.
"""


class SkillUpdateError(Exception):
    """Base class: the tool could not complete its work."""


class GitError(SkillUpdateError):
    """A git invocation failed, timed out, or was refused by an input allowlist."""


class ConfigError(SkillUpdateError):
    """The provider config or a provider manifest is missing, malformed, or inconsistent."""


class AdapterError(SkillUpdateError):
    """The GitHub adapter could not complete a call.

    Carries `status` (an HTTP status code when one is available) so the caller can distinguish
    the one failure mode that needs a specific human instruction -- 403 on pull-request
    creation, meaning the repository setting "Allow GitHub Actions to create and approve pull
    requests" is off -- from ordinary transport failures.
    """

    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status
