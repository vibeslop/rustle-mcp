# Security

Rustle runs on your machine and acts on what an agent passes it: it reads local files under its media roots, fetches URLs, and runs ffmpeg. Issues that matter most are ways around those limits, such as reading a file outside the media roots, a remote page or playlist reaching local files, or input that changes the ffmpeg command line.

Please report vulnerabilities privately through GitHub's "Report a vulnerability" button on the Security tab, not in a public issue. Include the source or arguments that trigger it and what they let you do.
