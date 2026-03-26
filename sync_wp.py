import requests
import pandas as pd
import json
from sqlalchemy import create_engine, text

# --- ΡΥΘΜΙΣΕΙΣ ---
DB_USER = "metabase_admin"
DB_PASS = "YourSecurePassword123!" # Βάλε το δικό σου
DB_HOST = "localhost"
DB_PORT = "5432"
DB_NAME = "metabase_db"

SITES = [
    {"name": "Opensource.ellak", "url": "https://opensource.ellak.gr"},
    {"name": "Opengov.ellak", "url": "https://opengov.ellak.gr"},
    {"name": "Mycontent.ellak", "url": "https://mycontent.ellak.gr"},
    {"name": "Opendata.ellak", "url": "https://opendata.ellak.gr"},
    {"name": "Openhardware.ellak", "url": "https://openhardware.ellak.gr"},
    {"name": "Openwifi.ellak", "url": "https://openwifi.ellak.gr"},
    {"name": "Oer.ellak", "url": "https://oer.ellak.gr"},
    {"name": "Openstandards.ellak", "url": "https://openstandards.ellak.gr"},
    {"name": "Odi.ellak", "url": "https://odi.ellak.gr"},
    {"name": "Obs.ellak", "url": "https://obs.ellak.gr"},
    {"name": "Legal.ellak", "url": "https://legal.ellak.gr"},
    {"name": "Smartcities.ellak", "url": "https://smartcities.ellak.gr"},
    {"name": "Openbusiness.ellak", "url": "https://openbusiness.ellak.gr"},
    {"name": "Advisory.ellak", "url": "https://advisory.ellak.gr"},
    {"name": "Opendesign.ellak", "url": "https://opendesign.ellak.gr"},
    {"name": "Legal.ellak", "url": "https://legal.ellak.gr"},
    {"name": "Privacy.ellak", "url": "https://privacy.ellak.gr"},
    {"name": "Edu.ellak", "url": "https://edu.ellak.gr"},
    {"name": "Creative Commons", "url": "https://creativecommons.ellak.gr"},
    {"name": "Glossapi", "url": "https://blog.glossapi.gr"},
]

engine = create_engine(f'postgresql://{DB_USER}:{DB_PASS}@{DB_HOST}:{DB_PORT}/{DB_NAME}')

def fetch_all_posts(site_name, base_url):
    print(f"--- Κατέβασμα: {site_name} ---")
    endpoint = f"{base_url}/wp-json/wp/v2/posts"
    all_posts = []
    page = 1
    
    while True:
        params = {'per_page': 100, 'page': page, '_embed': 1}
        try:
            response = requests.get(endpoint, params=params, timeout=20)
            if response.status_code != 200: break
            
            posts = response.json()
            if not posts: break
                
            for post in posts:
                # Ασφαλής λήψη ονόματος συγγραφέα
                author = "Unknown"
                try:
                    if '_embedded' in post and 'author' in post['_embedded']:
                        author = post['_embedded']['author'][0]['name']
                except (KeyError, IndexError):
                    pass

                all_posts.append({
                    "site_name": site_name,
                    "wp_id": post['id'],
                    "title": post['title']['rendered'],
                    "date": post['date'],
                    "link": post['link'],
                    "author_name": author,
                    # Μετατρέπουμε τη λίστα σε JSON string για να μην μπερδεύεται η Postgres
                    "categories": json.dumps(post.get('categories', []))
                })
            
            print(f"Σελίδα {page}: OK")
            page += 1
        except Exception as e:
            print(f"Σφάλμα στο {site_name}: {e}")
            break
            
    return all_posts

# --- MAIN ---
all_data = []
for site in SITES:
    data = fetch_all_posts(site['name'], site['url'])
    all_data.extend(data)

if all_data:
    df = pd.DataFrame(all_data)
    
    # --- Η ΔΙΟΡΘΩΣΗ ---
    # Αφαιρούμε τα διπλότυπα wp_id που μπορεί να προέκυψαν από το API pagination
    initial_len = len(df)
    df = df.drop_duplicates(subset=['wp_id'], keep='first')
    final_len = len(df)
    
    if initial_len != final_len:
        print(f"Αφαιρέθηκαν {initial_len - final_len} διπλότυπες εγγραφές.")
    # ------------------

    # Στέλνουμε τα δεδομένα στον temp πίνακα
    df.to_sql('wp_posts_temp', engine, if_exists='replace', index=False)
    
    with engine.begin() as conn:
        # Δημιουργία πίνακα αν δεν υπάρχει
        conn.execute(text("""
            CREATE TABLE IF NOT EXISTS all_articles (
                site_name TEXT,
                wp_id INTEGER PRIMARY KEY,
                title TEXT,
                date TIMESTAMP,
                link TEXT,
                author_name TEXT,
                categories JSONB
            );
        """))
        
        # Upsert Logic
        conn.execute(text("""
            INSERT INTO all_articles (site_name, wp_id, title, date, link, author_name, categories)
            SELECT site_name, wp_id, title, CAST(date AS TIMESTAMP), link, author_name, CAST(categories AS JSONB)
            FROM wp_posts_temp
            ON CONFLICT (wp_id) DO UPDATE SET
                title = EXCLUDED.title,
                date = EXCLUDED.date,
                link = EXCLUDED.link,
                author_name = EXCLUDED.author_name,
                categories = EXCLUDED.categories;
        """))
        
    print(f"\n✅ Επιτυχής ενημέρωση! Συνολικά μοναδικά άρθρα στη βάση: {len(df)}")
else:
    print("Δεν βρέθηκαν δεδομένα.")