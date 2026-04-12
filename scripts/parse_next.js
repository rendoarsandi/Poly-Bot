const fs = require('fs');

const html = fs.readFileSync('profile_raw.html', 'utf-8');

// The __next_f strings look like: self.__next_f.push([1,"...string..."])
const regex = /self\.__next_f\.push\(\[1,"(.*?[^\\])"]\)/g;
let match;
let combined = "";

while ((match = regex.exec(html)) !== null) {
    // Unescape the string
    let str = match[1]
        .replace(/\\"/g, '"')
        .replace(/\\\\/g, '\\')
        .replace(/\\n/g, '\n');
    combined += str;
}

fs.writeFileSync('next_data_combined.txt', combined);
console.log("Extracted combined next_f data. Length:", combined.length);
