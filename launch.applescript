set ghpat to system attribute "gh_pat"
if ghpat is equal to "" then
    display dialog "Environment variable gh_pat is not set." buttons {"OK"} default button "OK"
    return
end if

do shell script "cd ~/mcp-servers && echo " & quoted form of ghpat & " | docker login ghcr.io -u sheawinkler --password-stdin && export GITHUB_PAT=" & quoted form of ghpat & " && docker compose --env-file .env up -d"
